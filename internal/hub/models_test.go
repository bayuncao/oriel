package hub

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/task"
)

// MD01、MD02：agent 報告的模型記到它名下，重啟後還在；內容沒變不再寫。
func TestModelCatalog(t *testing.T) {
	env := newTestHub(t, nil)
	if got, err := env.h.Models(ctxb(), LocalOwner); err != nil || len(got) != 0 {
		t.Fatalf("fresh: %+v %v", got, err)
	}
	id := env.create(t, t.TempDir(), "say hi", modelLaunch())
	w := watch(t, env.h, id)
	w.next(task.TurnCompletedKind)
	env.h.waitReports()
	want := func(h *Hub) {
		t.Helper()
		got, err := h.Models(ctxb(), LocalOwner)
		if err != nil || len(got) != 1 {
			t.Fatalf("models %+v %v", got, err)
		}
		m := got[0]
		values := []string{}
		for _, c := range m.Choices {
			values = append(values, c.Value)
		}
		if m.Agent != "test" || m.Current != "fast" || !slices.Equal(values, []string{"fast", "deep", "broken"}) || m.ReportedAt.IsZero() {
			t.Fatalf("models %+v", m)
		}
	}
	want(env.h)
	env.h.Close()
	want(env.restart(t))
}

// MD03、MD04、MD11：設與清預設模型，只有桌面能設；不認得的 agent 回 agent.unknown_agent；
// 不在最近列表裡的模型照存。
func TestSetDefaultModel(t *testing.T) {
	l := newDiscoveryLauncher(t)
	env := newTestHub(t, func(o *Options) { o.Launcher = l })
	get := func() string {
		t.Helper()
		got, err := env.h.Models(ctxb(), LocalOwner)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range got {
			if m.Agent == "models" {
				return m.Default
			}
		}
		return ""
	}
	if err := env.h.SetDefaultModel(ctxb(), LocalOwner, "models", " deep "); err != nil || get() != "deep" {
		t.Fatalf("set: %v, default %q", err, get())
	}
	if err := env.h.SetDefaultModel(ctxb(), LocalOwner, "models", "not-reported-yet"); err != nil || get() != "not-reported-yet" {
		t.Fatalf("MD11 unreported model: %v, default %q", err, get())
	}
	if err := env.h.SetDefaultModel(ctxb(), LocalOwner, "models", ""); err != nil || get() != "" {
		t.Fatalf("MD04 clear: %v, default %q", err, get())
	}
	if err := env.h.SetDefaultModel(ctxb(), LocalOwner, "nobody", "deep"); errCode(err) != string(agent.CodeUnknownAgent) {
		t.Fatalf("MD11 unknown agent: %v", err)
	}
	if err := env.h.SetDefaultModel(ctxb(), Principal{Device: "phone"}, "models", "deep"); errCode(err) != string(CodeNeedsDesktop) {
		t.Fatalf("remote: %v", err)
	}
}

// MD05、MD07、MD09：新會話的啟動參數帶預設模型；在列表裡就在第一條 Prompt 之前換成它；
// 不在列表裡照 agent 自己的，記一條說明，Prompt 照常送出。
func TestDefaultModelOnNewSession(t *testing.T) {
	l := newDiscoveryLauncher(t)
	for _, tc := range []struct {
		name, model string
		log         []string // agent 依序收到的
		failed      string   // ModelSwitchFailed 的代碼；"" = 沒有
	}{
		{name: "MD05 offered", model: "deep", log: []string{"model: deep", "say hi"}},
		{name: "MD07 not offered", model: "huge", log: []string{"say hi"}, failed: task.CodeDefaultModelUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestHub(t, func(o *Options) { o.Launcher = l })
			if err := env.h.SetDefaultModel(ctxb(), LocalOwner, "models", tc.model); err != nil {
				t.Fatal(err)
			}
			id, _, err := env.h.CreateTaskWithAgent(ctxb(), LocalOwner, t.TempDir(), task.CreateTask{Title: "t", Brief: "say hi"}, "models")
			if err != nil {
				t.Fatal(err)
			}
			w := watch(t, env.h, id)
			if lc := w.next(task.SessionCreatedKind).SessionCreated.Launch; lc.Model != tc.model {
				t.Fatalf("launch model %q, want %q", lc.Model, tc.model)
			}
			if tc.failed != "" {
				e := w.next(task.ModelSwitchFailedKind)
				if p := e.ModelSwitchFailed; p.Value != tc.model || p.Problem.Code != tc.failed {
					t.Fatalf("switch failed %+v", p)
				}
			}
			w.next(task.TurnCompletedKind)
			if got := env.agentLog(t); !slices.Equal(got, tc.log) {
				t.Fatalf("agent got %q, want %q", got, tc.log)
			}
			// MD09：改預設只影響之後的會話。
			if err := env.h.SetDefaultModel(ctxb(), LocalOwner, "models", "fast"); err != nil {
				t.Fatal(err)
			}
			snap, err := env.h.Snapshot(ctxb(), LocalOwner, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range snap.Sessions {
				if x.Launch.Model != tc.model {
					t.Fatalf("existing session launch model changed to %q", x.Launch.Model)
				}
			}
		})
	}
}

// Actor 記模型報告時不等 cfgWrite：CreateTask 登記新 Workspace 時拿著 cfgWrite 等 actor 回答
// （otherRunning），actor 若等 cfgWrite 就兩邊互等。拿著 cfgWrite 時報告照樣送到訂閱者，放開後才寫進設定。
func TestModelReportDoesNotWaitForConfigWrites(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", modelLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	w.next(task.TurnCompletedKind)
	env.h.waitReports()

	env.h.cfgWrite.Lock()
	unlock := sync.OnceFunc(env.h.cfgWrite.Unlock)
	defer unlock()
	if err := env.setModel(t, id, sid, "deep"); err != nil {
		t.Fatal(err)
	}
	reportedModel(w, "deep") // 舊的寫法在這裡等不到：actor 卡在 cfgWrite，還沒推給訂閱者
	if _, err := env.h.Snapshot(ctxb(), LocalOwner, id); err != nil {
		t.Fatal(err) // actor 還在回答
	}
	if got, _ := env.h.Models(ctxb(), LocalOwner); len(got) != 1 || got[0].Current != "fast" {
		t.Fatalf("written while cfgWrite was held: %+v", got)
	}
	unlock()
	env.h.waitReports()
	if got, _ := env.h.Models(ctxb(), LocalOwner); len(got) != 1 || got[0].Current != "deep" {
		t.Fatalf("after unlock: %+v", got)
	}
}

// 不帶列表的報告（agent 沒給過列表時換模型的回覆）不蓋掉記下的列表；交來還沒寫的也一樣。
func TestModelReportKeepsChoices(t *testing.T) {
	at := time.Unix(100, 0)
	listed := modelReport{choices: []task.ModelChoice{{Value: "fast"}, {Value: "deep"}}, current: "fast", at: at}
	bare := modelReport{current: "deep", at: at.Add(time.Second)}
	values := func(m AgentModels) []string {
		var out []string
		for _, c := range m.Choices {
			out = append(out, c.Value)
		}
		return out
	}
	var m AgentModels
	listed.apply(&m)
	bare.apply(&m)
	if !slices.Equal(values(m), []string{"fast", "deep"}) || m.Current != "deep" || !m.ReportedAt.Equal(bare.at) {
		t.Fatalf("apply: %+v", m)
	}
	var r modelReports
	r.init()
	r.add("a", listed)
	r.add("a", bare)
	var pending AgentModels
	r.pending["a"].apply(&pending)
	if !slices.Equal(values(pending), []string{"fast", "deep"}) || pending.Current != "deep" {
		t.Fatalf("pending: %+v", pending)
	}
}
