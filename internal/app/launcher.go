package app

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// probeTimeout 是一次能否恢復的探測的時限：initialize 很快，不必等到 Session
// 啟動的 60 秒；探測期間建 Task 的呼叫在等它。
const probeTimeout = 15 * time.Second

// launcher 是 hub.Launcher（M2-2）：確認 agent 能啟動，用選的 Profile 解析成
// Launch Snapshot（m4.md 第 3 節）；ACP agent 能否恢復由探測決定（D98），成功的結果按可執行檔
// 路徑與版本記在記憶體裡，同時來的解析共用一次探測（含它的失敗）。
type launcher struct {
	disc  *agent.Discovery
	defs  func() []agent.Definition                                                   // 目前的目錄（Agents.Definitions，執行中可變，AC04）
	find  func(task.AgentID, task.ProfileID) (agent.Definition, agent.Profile, error) // Agents.Lookup
	probe func(context.Context, proc.Command) (task.Capabilities, error)              // acp.Runtime.Capabilities
	log   *slog.Logger

	mu       sync.Mutex
	caps     map[capsKey]task.Capabilities
	inflight map[capsKey]*probeCall
	// waiting 在呼叫者開始等別人的探測時呼叫；僅測試（確定性地等到併發）。
	waiting func()

	// leaseMu 保護 leased，也讓「讀目錄、登記密鑰」成為一步：清理在換掉目錄之後才看 leased，
	// 讀到舊目錄的解析那時已登記了（m4.md AC37）。
	leaseMu sync.Mutex
	leased  map[string]int // 解析出、還沒寫進事件流的快照引用的密鑰 → 幾個解析
	// released 在解除登記之後以這次登記的密鑰呼叫（app 據此在它們待清理時再清理一次）；nil 時不呼叫。
	released func(keys []string)
}

// capsKey 是探測結果的快取鍵：同一個可執行檔換了版本就重新探測（P07）。可執行檔不代表
// agent：Registry 的 npx 條目共用 npx、自訂 agent 可以共用直譯器，所以鍵也含 Agent ID 與
// 啟動參數、環境變數（更新條目換了套件版本、改了自訂 agent 的參數或環境變數都重新探測）。
type capsKey struct {
	agent         task.AgentID
	path, version string
	launch        string // Launch.Args 與 Launch.Env（名稱、值或密鑰的鍵），以 NUL 分隔
}

func newCapsKey(def agent.Definition, inst agent.Installation) capsKey {
	parts := slices.Clone(def.Launch.Args)
	for _, e := range def.Launch.Env {
		parts = append(parts, "\x01"+e.Name, e.Value, e.Secret)
	}
	return capsKey{agent: def.ID, path: inst.Path, version: inst.Version, launch: strings.Join(parts, "\x00")}
}

// probeCall 是一次進行中的探測；done 關閉後 caps、err 是它的結果。
type probeCall struct {
	done chan struct{}
	caps task.Capabilities
	err  error
}

func newLauncher(disc *agent.Discovery, g *Agents,
	probe func(context.Context, proc.Command) (task.Capabilities, error), log *slog.Logger) *launcher {
	return &launcher{disc: disc, defs: g.Definitions, find: g.Lookup, probe: probe, log: log,
		caps: map[capsKey]task.Capabilities{}, inflight: map[capsKey]*probeCall{}, leased: map[string]int{}}
}

// Leased 回報密鑰 key 是否被還沒 release 的解析引用（Agents 清理待清理的密鑰時跳過它，AC37）。
func (l *launcher) Leased(key string) bool {
	l.leaseMu.Lock()
	defer l.leaseMu.Unlock()
	return l.leased[key] > 0
}

// lease 在目錄裡找 id 與它的 Profile pid，並把它們的快照會引用的密鑰登記為正在用；找不到時是
// agent.unknown_agent 或 agent.unknown_profile。release 解除登記，呼叫一次。
func (l *launcher) lease(id task.AgentID, pid task.ProfileID) (def agent.Definition, p agent.Profile, release func(), err error) {
	l.leaseMu.Lock()
	defer l.leaseMu.Unlock()
	if def, p, err = l.find(id, pid); err != nil {
		return agent.Definition{}, agent.Profile{}, nil, err
	}
	var keys []string
	for _, v := range append(slices.Clone(def.Launch.Env), p.Env...) {
		if v.Secret != "" {
			keys = append(keys, v.Secret)
			l.leased[v.Secret]++
		}
	}
	return def, p, sync.OnceFunc(func() {
		l.leaseMu.Lock()
		for _, k := range keys {
			if l.leased[k]--; l.leased[k] == 0 {
				delete(l.leased, k)
			}
		}
		l.leaseMu.Unlock()
		if len(keys) > 0 && l.released != nil {
			l.released(keys)
		}
	}), nil
}

// CheckAgent 在 agent 在目錄裡時回 nil，否則回 agent.unknown_agent（P09；hub 設預設模型時用，MD11）。
func (l *launcher) CheckAgent(id task.AgentID) error {
	if slices.ContainsFunc(l.defs(), func(d agent.Definition) bool { return d.ID == id }) {
		return nil
	}
	return agent.ErrUnknownAgent
}

// Launch 先以 Verify 重新確認 agent（P03），再用 Profile pid（空著是預設的）解析（P01、P02、PO06–PO18）。
// ctx 結束時回它的錯誤，不會以「不能恢復」建出 Task。成功時快照引用的密鑰在 hub 呼叫
// release 之前算正在用（hub.Launcher）；失敗時自己解除。
func (l *launcher) Launch(ctx context.Context, id task.AgentID, pid task.ProfileID) (task.LaunchSnapshot, func(), error) {
	def, p, release, err := l.lease(id, pid)
	if err != nil {
		return task.LaunchSnapshot{}, nil, err // P09、PO17
	}
	snap, err := l.resolve(ctx, def, p)
	if err != nil {
		release()
		return task.LaunchSnapshot{}, nil, err
	}
	return snap, release, nil
}

func (l *launcher) resolve(ctx context.Context, def agent.Definition, p agent.Profile) (task.LaunchSnapshot, error) {
	inst, err := l.disc.Verify(ctx, def.ID)
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
	return agent.Launch(def, inst, p, caps), nil
}

// capabilities 回傳探測結果：快取裡有就用（P06）；別人正在探測就等它，連它的
// 失敗一起用（P05）；否則自己探測。探測不跟著某個呼叫者的 ctx：一個呼叫者離開
// 不會讓其他人拿到失敗。失敗時這次按不能恢復，不記下來，下次再探測（P08）。
// 只有呼叫者自己的 ctx 結束時回錯。
func (l *launcher) capabilities(ctx context.Context, def agent.Definition, inst agent.Installation) (task.Capabilities, error) {
	key := newCapsKey(def, inst)
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
// initialize（不登入、不花額度）。用 agent 自己的參數與環境變數（沒改過的預設 Profile）：能力屬於
// agent 的版本，不隨 Profile 變（快取鍵也不含 Profile）。
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
