//go:build unix

package hub

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// 換模型（m2.md V80、V83、V85、V86，D118）：測試 agent 報告 fast、deep、broken，每次啟動都從 fast 開始
// （像 OpenCode 接回後回到預設），broken 一律拒絕。

func modelLaunch() task.LaunchSnapshot {
	l := testLaunch()
	l.Env = []task.EnvVar{{Name: "ORIELHUB_TEST_MODELS", Value: "fast,deep,broken"}}
	return l
}

func (env *testEnv) setModel(t *testing.T, id task.TaskID, sid task.SessionID, v string) error {
	t.Helper()
	_, err := env.h.Command(context.Background(), LocalOwner, id, task.Command{
		Kind: task.CmdSetModel, Session: sid, SetModel: &task.SetModel{Value: v}})
	return err
}

// agentLog 是測試 agent 收到的 prompt 與換模型（model: 開頭），依收到的順序。
func (env *testEnv) agentLog(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(env.dir, "prompts.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// reportedModel 等 agent 報告目前是 v 的 ModelReported。
func reportedModel(w *watcher, v string) task.Event {
	return w.until(func(e task.Event) bool {
		return e.Kind == task.ModelReportedKind && e.ModelReported.Setting != nil && e.ModelReported.Setting.Current == v
	})
}

// V77、V80、V83：啟動時的報告在綁定之前；換成 deep 後 agent 確認；換成 agent 拒絕的模型時記 ModelSwitchFailed，
// 不掛起、不當作退出，模型留在 deep。
func TestModelSwitch(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", modelLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	first := w.until(func(e task.Event) bool {
		return e.Kind == task.ModelReportedKind || e.Kind == task.NativeSessionBoundKind
	})
	if first.Kind != task.ModelReportedKind || !first.ModelReported.Start || first.ModelReported.Setting.Current != "fast" {
		t.Fatalf("first %s %+v, want the start report before binding", first.Kind, first.ModelReported)
	}
	w.next(task.TurnCompletedKind)

	if err := env.setModel(t, id, sid, "deep"); err != nil {
		t.Fatal(err)
	}
	if e := reportedModel(w, "deep"); e.ModelReported.Start {
		t.Fatalf("switch reported as start: %+v", e.ModelReported)
	}

	if err := env.setModel(t, id, sid, "broken"); err != nil {
		t.Fatal(err)
	}
	f := w.next(task.ModelSwitchFailedKind).ModelSwitchFailed
	if f.Value != "broken" || f.Problem.Code != string(CodeEffectFailed) {
		t.Fatalf("failed %+v", f)
	}
	snap := snapshotOf(t, env.h, id)
	x := snap.State.Sessions[sid]
	if x.Model.Current != "deep" || x.ModelWanted != "" || x.ModelProblem == nil || x.Status != task.SessionIdle {
		t.Fatalf("session %+v model %+v", x, x.Model)
	}
	if _, ok := snap.State.Failed["set_model:"+task.EffectKey(sid)]; ok {
		t.Fatal("set_model suspended")
	}
	if got := env.agentLog(t); !slices.Equal(got, []string{"say hi", "model: deep", "model: broken"}) {
		t.Fatalf("agent got %q", got)
	}
}

// V86：重啟後 agent 接回時回到 fast；下一條訊息送出之前先設回 deep。
func TestModelReappliedAfterRestart(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", modelLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnCompletedKind)
	if err := env.setModel(t, id, sid, "deep"); err != nil {
		t.Fatal(err)
	}
	reportedModel(w, "deep")
	w.cancel()
	before, _ := env.store.Read(context.Background(), id, 0, 0)
	env.h.Close()
	env.h = env.restart(t)

	env.submit(t, id, sid, "say again")
	w2 := watch(t, env.h, id)
	w2.until(func(e task.Event) bool {
		switch e.Kind {
		case task.TurnFailedKind, task.ProcessExitedKind, task.ModelSwitchFailedKind:
			if e.Seq <= task.Seq(len(before)) {
				return false
			}
			t.Fatalf("unexpected %s", e.Kind)
		}
		return e.Kind == task.TurnCompletedKind && e.Seq > task.Seq(len(before)) // 跳過重啟前的回放
	})
	if got := env.agentLog(t); !slices.Equal(got, []string{"say hi", "model: deep", "model: deep", "say again"}) {
		t.Fatalf("agent got %q, want deep set again before the prompt", got)
	}
	if x := snapshotOf(t, env.h, id).State.Sessions[sid]; x.Model.Current != "deep" || x.ModelWanted != "deep" {
		t.Fatalf("model %+v wanted %q", x.Model, x.ModelWanted)
	}
}

// V85：重啟後閒著（行程不在）時選模型：以 Resume 接回 agent 去設，不必等下一條訊息。
func TestModelResumesWhenLost(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", modelLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	nid := w.next(task.NativeSessionBoundKind).NativeSessionBound.NativeID
	w.next(task.TurnCompletedKind)
	w.cancel()
	env.h.Close()
	env.h = env.restart(t)

	w2 := watch(t, env.h, id)
	if err := env.setModel(t, id, sid, "deep"); err != nil {
		t.Fatal(err)
	}
	reportedModel(w2, "deep")
	snap := snapshotOf(t, env.h, id)
	if c, ok := snap.Live.Conns[sid]; !ok || c.NativeID != nid {
		t.Fatalf("conns %+v, want resumed %q", snap.Live.Conns, nid)
	}
	if got := env.agentLog(t); !slices.Equal(got, []string{"say hi", "model: deep"}) {
		t.Fatalf("agent got %q", got)
	}
}

// V85：換模型的途中歸檔：被停掉的行程沒回應不算沒能換成，選擇留著；取消歸檔後接回 agent 再設上（D117）。
func TestModelSwitchSurvivesArchive(t *testing.T) {
	env := newTestHub(t, nil)
	gate := filepath.Join(env.dir, "model-gate")
	l := modelLaunch()
	l.Env = append(l.Env, task.EnvVar{Name: "ORIELHUB_TEST_MODEL_GATE", Value: gate})
	exited := make(chan struct{}, 1)
	env.h.hooks.fact = func(_ task.TaskID, c task.Command) {
		if c.Kind == task.FactExited {
			select {
			case exited <- struct{}{}:
			default:
			}
		}
	}
	id, sid, _, w := archiveIdle(t, env, l)
	if err := env.setModel(t, id, sid, "deep"); err != nil {
		t.Fatal(err)
	}
	if snap := snapshotOf(t, env.h, id); !snap.Live.InFlight[task.EffectKey("set_model:"+string(sid))] {
		t.Fatalf("in flight %v, want set_model", snap.Live.InFlight)
	}
	env.command(t, id, task.CmdArchive)
	select {
	case <-exited: // 被停掉的行程回報了退出
	case <-time.After(testWait):
		t.Fatal("the stopped agent never reported its exit")
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env.command(t, id, task.CmdUnarchive)
	w.until(func(e task.Event) bool {
		switch e.Kind {
		case task.ModelSwitchFailedKind, task.ProcessExitedKind:
			t.Fatalf("unexpected %s: %+v", e.Kind, e)
		}
		return e.Kind == task.ModelReportedKind && !e.ModelReported.Start && e.ModelReported.Setting.Current == "deep"
	})
	if x := snapshotOf(t, env.h, id).State.Sessions[sid]; x.ModelWanted != "deep" || x.ModelProblem != nil {
		t.Fatalf("wanted %q problem %+v", x.ModelWanted, x.ModelProblem)
	}
	// 停掉之前，舊行程不一定已收到第一次的請求；接回的那個一定收到了。
	if got := env.agentLog(t); got[0] != "say hi" || len(got) < 2 || len(got) > 3 || slices.ContainsFunc(got[1:], func(l string) bool { return l != "model: deep" }) {
		t.Fatalf("agent got %q", got)
	}
}
