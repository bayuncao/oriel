package app

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// probeTimeout 是一次能否恢復的探測的時限：initialize 很快，不必等到 Session
// 啟動的 60 秒；探測期間建 Task 的呼叫在等它。
const probeTimeout = 15 * time.Second

// launcher 是 hub.Launcher（M2-2）：確認 agent 能啟動，用預設 Profile 解析成
// Launch Snapshot；ACP agent 能否恢復由探測決定（D98），成功的結果按可執行檔
// 路徑與版本記在記憶體裡，同時來的解析共用一次探測（含它的失敗）。
type launcher struct {
	disc  *agent.Discovery
	defs  []agent.Definition
	probe func(context.Context, proc.Command) (task.Capabilities, error) // acp.Runtime.Capabilities
	log   *slog.Logger

	mu       sync.Mutex
	caps     map[capsKey]task.Capabilities
	inflight map[capsKey]*probeCall
	// waiting 在呼叫者開始等別人的探測時呼叫；僅測試（確定性地等到併發）。
	waiting func()
}

// capsKey 是探測結果的快取鍵：同一個可執行檔換了版本就重新探測（P07）。
type capsKey struct{ path, version string }

// probeCall 是一次進行中的探測；done 關閉後 caps、err 是它的結果。
type probeCall struct {
	done chan struct{}
	caps task.Capabilities
	err  error
}

func newLauncher(disc *agent.Discovery, defs []agent.Definition,
	probe func(context.Context, proc.Command) (task.Capabilities, error), log *slog.Logger) *launcher {
	return &launcher{disc: disc, defs: defs, probe: probe, log: log,
		caps: map[capsKey]task.Capabilities{}, inflight: map[capsKey]*probeCall{}}
}

// CheckAgent 在 agent 在目錄裡時回 nil，否則回 agent.unknown_agent（P09；hub 設預設模型時用，MD11）。
func (l *launcher) CheckAgent(id task.AgentID) error {
	if slices.ContainsFunc(l.defs, func(d agent.Definition) bool { return d.ID == id }) {
		return nil
	}
	return agent.ErrUnknownAgent
}

// Launch 先以 Verify 重新確認 agent（P03），再用預設 Profile 解析（P01、P02）。
// ctx 結束時回它的錯誤，不會以「不能恢復」建出 Task。
func (l *launcher) Launch(ctx context.Context, id task.AgentID) (task.LaunchSnapshot, error) {
	var def agent.Definition
	for _, d := range l.defs {
		if d.ID == id {
			def = d
		}
	}
	if def.ID == "" {
		return task.LaunchSnapshot{}, agent.ErrUnknownAgent // P09
	}
	inst, err := l.disc.Verify(ctx, id)
	if err != nil {
		return task.LaunchSnapshot{}, err
	}
	if err := inst.Unready(); err != nil {
		return task.LaunchSnapshot{}, err
	}
	// 原生協議的能力寫在 Definition 裡；ACP agent 的要對本機裝的版本探測（D98）。
	caps := def.Caps
	if def.Protocol == task.ProtocolACP {
		if caps, err = l.capabilities(ctx, def, inst); err != nil {
			return task.LaunchSnapshot{}, err
		}
	}
	return agent.Launch(def, inst, agent.DefaultProfile(def), caps), nil
}

// capabilities 回傳探測結果：快取裡有就用（P06）；別人正在探測就等它，連它的
// 失敗一起用（P05）；否則自己探測。探測不跟著某個呼叫者的 ctx：一個呼叫者離開
// 不會讓其他人拿到失敗。失敗時這次按不能恢復，不記下來，下次再探測（P08）。
// 只有呼叫者自己的 ctx 結束時回錯。
func (l *launcher) capabilities(ctx context.Context, def agent.Definition, inst agent.Installation) (task.Capabilities, error) {
	key := capsKey{inst.Path, inst.Version}
	l.mu.Lock()
	if c, ok := l.caps[key]; ok {
		l.mu.Unlock()
		return c, nil
	}
	call, running := l.inflight[key]
	if !running {
		call = &probeCall{done: make(chan struct{})}
		l.inflight[key] = call
		go l.run(key, call, def, inst)
	} else if l.waiting != nil {
		l.waiting()
	}
	l.mu.Unlock()
	select {
	case <-call.done:
	case <-ctx.Done():
		return task.Capabilities{}, ctx.Err()
	}
	if call.err != nil {
		return task.Capabilities{}, nil // 這次按不能恢復（D103）
	}
	return call.caps, nil
}

// run 執行一次探測並交出結果；成功的才記進快取。
func (l *launcher) run(key capsKey, call *probeCall, def agent.Definition, inst agent.Installation) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	call.caps, call.err = l.probeOnce(ctx, def, inst)
	l.mu.Lock()
	delete(l.inflight, key)
	if call.err == nil {
		l.caps[key] = call.caps
	}
	l.mu.Unlock()
	close(call.done)
	if call.err != nil {
		l.log.Warn("app: capability probe failed; sessions of this agent cannot be resumed until a probe succeeds",
			"agent", def.ID, "path", inst.Path, "version", inst.Version, "err", call.err)
	}
}

// probeOnce 在一個用完即刪的暫存目錄裡、以環境快照啟動 agent，發一次
// initialize（不登入、不花額度）。
func (l *launcher) probeOnce(ctx context.Context, def agent.Definition, inst agent.Installation) (task.Capabilities, error) {
	env, err := l.disc.Env(ctx)
	if err != nil {
		return task.Capabilities{}, err
	}
	snap := agent.Launch(def, inst, agent.DefaultProfile(def), task.Capabilities{})
	for _, v := range snap.Env {
		if v.Secret == "" { // 密鑰不為探測取值
			env = env.With(v.Name + "=" + v.Value)
		}
	}
	dir, err := os.MkdirTemp("", "orielhub-probe-")
	if err != nil {
		return task.Capabilities{}, err
	}
	defer os.RemoveAll(dir)
	return l.probe(ctx, proc.Command{Path: snap.Executable, Args: snap.Args, Env: env, Dir: dir})
}
