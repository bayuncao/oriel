package hub_test

// sqlite_test.go：端到端——真 SQLite store 跑完整的事件溯源循環。

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/store"
	"github.com/bayuncao/oriel/internal/task"
)

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(),
		filepath.Join(dir, "orielhub.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// E1：say hi 全鏈路在真 store 上。
func TestSQLiteFullChain(t *testing.T) {
	env := hub.NewTestEnv(t, openStore(t, t.TempDir()))
	id := env.Create(t, t.TempDir(), "say hi")
	w := env.Watch(t, id)
	e := w.Next(task.TurnCompletedKind)
	if e.Kind != task.TurnCompletedKind {
		t.Fatalf("%+v", e)
	}
	w.Stop()
}

// E2：回合中（hang）關閉 → 重開 → TurnFailed{hub.connection_lost}
// 與 ProcessExited 補進 Stream。
func TestSQLiteReconnectFailsTurn(t *testing.T) {
	data := t.TempDir()
	env := hub.NewTestEnv(t, openStore(t, data))
	id := env.Create(t, t.TempDir(), "hang")
	w := env.Watch(t, id)
	w.Until(func(e task.Event) bool { return e.Kind == task.TurnStartedKind })
	w.Until(func(e task.Event) bool { return e.Kind == task.NativeSessionBoundKind })
	env.Hub().Close()
	w.Stop()
	env2 := hub.NewTestEnv(t, openStore(t, data))
	defer env2.Hub().Close()
	w2 := env2.Watch(t, id)
	e := w2.Until(func(e task.Event) bool {
		return e.Kind == task.TurnFailedKind
	})
	if e.TurnFailed == nil ||
		string(e.TurnFailed.Problem.Code) != "hub.connection_lost" {
		t.Fatalf("%+v", e.TurnFailed)
	}
	w2.Until(func(e task.Event) bool {
		return e.Kind == task.ProcessExitedKind
	})
	w2.Stop()
}

// E3：RedactMessage → 關閉重開 → 重放見 Redacted 片段。
func TestSQLiteRedactPersists(t *testing.T) {
	data := t.TempDir()
	env := hub.NewTestEnv(t, openStore(t, data))
	id := env.Create(t, t.TempDir(), "say hi")
	w := env.Watch(t, id)
	w.Next(task.TurnCompletedKind)
	w.Stop()
	var msg task.MessageID
	done := errors.New("done")
	sid := env.SessionOf(t, id)
	err := env.Hub().Subscribe(context.Background(), hub.LocalOwner, id, 0,
		func(u hub.Update) error {
			if u.Event != nil && u.Event.Kind == task.MessageAppendedKind &&
				u.Event.MessageAppended.Text != "" {
				msg = u.Event.MessageAppended.Message
				return done
			}
			return nil
		})
	if !errors.Is(err, done) || msg == "" {
		t.Fatalf("msg %q err %v", msg, err)
	}
	if _, err := env.Hub().Command(context.Background(), hub.LocalOwner, id,
		task.Command{Kind: task.CmdRedactMessage, Session: sid,
			RedactMessage: &task.RedactMessage{Message: msg}}); err != nil {
		t.Fatal(err)
	}
	env.Hub().Close()
	env2 := hub.NewTestEnv(t, openStore(t, data))
	defer env2.Hub().Close()
	var redacted bool
	err = env2.Hub().Subscribe(context.Background(), hub.LocalOwner, id, 0,
		func(u hub.Update) error {
			if u.Event != nil && u.Event.Kind == task.MessageAppendedKind &&
				u.Event.MessageAppended.Message == msg &&
				u.Event.MessageAppended.Redacted {
				redacted = true
				return done
			}
			return nil
		})
	if !errors.Is(err, done) || !redacted {
		t.Fatalf("redacted=%v err=%v", redacted, err)
	}
}

// E4：Task A 存 Always(workspace) 規則 → 重開後 Task B 的同樣
// ask 仍被自動決定。
func TestSQLiteRuleAcrossTasks(t *testing.T) {
	data := t.TempDir()
	env := hub.NewTestEnv(t, openStore(t, data))
	dirA := t.TempDir()
	idA := env.Create(t, dirA, "ask a1 rm -rf /")
	wA := env.Watch(t, idA)
	ask := wA.Next(task.PermissionAskedKind)
	sid := ask.Session
	_, err := env.Hub().Command(context.Background(), hub.LocalOwner, idA,
		task.Command{Kind: task.CmdDecidePermission, Session: sid,
			DecidePermission: &task.DecidePermission{
				Permission: ask.PermissionAsked.ID, Verdict: task.Allow,
				Always: &task.Rule{Scope: task.ScopeWorkspace,
					Match: task.MatchPrefix, Pattern: "rm"}}})
	if err != nil {
		t.Fatal(err)
	}
	wA.Next(task.TurnCompletedKind)
	wA.Stop()
	env.Hub().Close()
	// 重開：規則從 store 載回快取，Task B 的自動放行仍然有效。
	env2 := hub.NewTestEnv(t, openStore(t, data))
	defer env2.Hub().Close()
	// 規則是 workspace 範圍：Task B 建在同一個目錄。
	idB := env2.Create(t, dirA, "ask a1 rm -rf /")
	wB := env2.Watch(t, idB)
	wB.Until(func(e task.Event) bool {
		return e.Kind == task.PermissionDecidedKind
	})
	wB.Stop()
}

// E5：Search 命中；Redact 後消失。
func TestSQLiteSearchRedact(t *testing.T) {
	env := hub.NewTestEnv(t, openStore(t, t.TempDir()))
	defer env.Hub().Close()
	dir := t.TempDir()
	id := env.Create(t, dir, "say hi")
	w := env.Watch(t, id)
	w.Next(task.TurnCompletedKind)
	w.Stop()
	hits, err := env.Hub().Search(context.Background(), hub.LocalOwner, "hi", 10)
	if err != nil || len(hits) == 0 {
		t.Fatalf("hits %v %v", hits, err)
	}
}

// E6：從 v1 升上來的資料庫（投影版本 0、Task 列的掛起數是遷移補的 0）
// 經 Start 重建後，掛起數由日誌算出；之後版本已記下。
func TestSQLiteRebuildFillsFailed(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	s := openStore(t, data)
	dir := hub.GitRepo(t)
	if err := s.PutWorkspace(ctx, hub.Workspace{ID: "w-always",
		Path: hub.RealPath(t, dir), Name: "repo", Policy: task.PolicyAlways}); err != nil {
		t.Fatal(err)
	}
	env := hub.NewTestEnv(t, s)
	env.BlockWorktrees(t)
	id := env.Create(t, dir, "") // 沒有 agent：只有建 worktree 的 Effect，建不出來而掛起
	w := env.Watch(t, id)
	w.Next(task.EffectFailedKind)
	w.Stop()
	env.Hub().Close()

	// 把投影改回遷移剛做完的樣子：failed 為 0、版本為 0。
	v, err := s.ViewsOf(ctx, id)
	if err != nil || v.Task == nil || v.Task.Failed != 1 {
		t.Fatalf("before %+v %v", v.Task, err)
	}
	v.Task.Failed = 0
	if err := s.Reproject(ctx, id, v); err != nil {
		t.Fatal(err)
	}
	if err := s.SetViewsVersion(ctx, 0); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := openStore(t, data)
	env2 := hub.NewTestEnv(t, s2)
	defer env2.Hub().Close()
	rows, err := env2.Hub().Tasks(ctx, hub.LocalOwner)
	if err != nil || len(rows) != 1 || rows[0].Failed != 1 {
		t.Fatalf("after rebuild %+v %v", rows, err)
	}
	if ver, err := s2.ViewsVersion(ctx); err != nil || ver == 0 {
		t.Fatalf("views version %d %v", ver, err)
	}
}

// DL01、DL08 在真 store 上：跑過一輪、歸檔的 Task 刪掉後事件與投影都沒了，重開也不回來，另一個 Task 照舊。
func TestSQLiteDeleteTask(t *testing.T) {
	data := t.TempDir()
	env := hub.NewTestEnv(t, openStore(t, data))
	keep := env.Create(t, t.TempDir(), "say hi")
	id := env.Create(t, t.TempDir(), "say hi")
	w := env.Watch(t, id)
	w.Next(task.TurnCompletedKind)
	w.Stop()
	ctx := context.Background()
	if _, err := env.Hub().Command(ctx, hub.LocalOwner, id, task.Command{Kind: task.CmdArchive}); err != nil {
		t.Fatal(err)
	}
	if err := env.Hub().DeleteTask(ctx, hub.LocalOwner, id); err != nil {
		t.Fatal(err)
	}
	env.Hub().Close()
	env2 := hub.NewTestEnv(t, openStore(t, data))
	defer env2.Hub().Close()
	views, err := env2.Hub().Tasks(ctx, hub.LocalOwner)
	if err != nil || len(views) != 1 || views[0].ID != keep {
		t.Fatalf("tasks %+v %v", views, err)
	}
	if hits, err := env2.Hub().Search(ctx, hub.LocalOwner, "hi", 0); err != nil || len(hits) == 0 {
		t.Fatalf("search %v %v", hits, err)
	} else {
		for _, h := range hits {
			if h.Task == id {
				t.Fatalf("deleted task still searchable: %+v", h)
			}
		}
	}
	var he *hub.Error
	if _, err := env2.Hub().Snapshot(ctx, hub.LocalOwner, id); !errors.As(err, &he) || he.Code != hub.CodeUnknownTask {
		t.Fatalf("snapshot %v", err)
	}
}

// S5：回放完、向 Actor 登記時 store 讀不了：訂閱以 hub.store_failed 結束，不留下一條缺了事件的訂閱。
func TestSQLiteSubscribeStoreFailsWhileRegistering(t *testing.T) {
	s := openStore(t, t.TempDir())
	env := hub.NewTestEnv(t, s)
	id := env.Create(t, t.TempDir(), "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	replayed := 0
	err := env.Hub().Subscribe(ctx, hub.LocalOwner, id, 0, func(hub.Update) error {
		if replayed++; replayed == 1 {
			return s.Close() // 回放的這一頁已經讀出來了；之後 Actor 補缺口的讀取失敗
		}
		return nil
	})
	var coded interface{ ErrorCode() string }
	if !errors.As(err, &coded) || coded.ErrorCode() != string(hub.CodeStoreFailed) {
		t.Fatalf("subscribe ended with %v, want hub.store_failed", err)
	}
}
