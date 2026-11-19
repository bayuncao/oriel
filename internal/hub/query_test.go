package hub

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

var ctxb = context.Background

// Q1：Tasks 依 UpdatedAt 新到舊，欄位與投影一致。
func TestTasksOrdering(t *testing.T) {
	env := newTestHub(t, nil)
	id1 := env.create(t, t.TempDir(), "", testLaunch())
	env.clock.Advance(time.Hour)
	id2 := env.create(t, t.TempDir(), "", testLaunch())
	tv, err := env.h.Tasks(ctxb(), LocalOwner)
	if err != nil || len(tv) != 2 {
		t.Fatalf("tasks %v %v", tv, err)
	}
	if tv[0].ID != id2 || tv[1].ID != id1 {
		t.Fatalf("order %v", tv)
	}
	v, _ := env.store.ViewsOf(ctxb(), id2)
	if !reflect.DeepEqual(*v.Task, tv[0]) {
		t.Fatalf("view %+v vs %+v", v.Task, tv[0])
	}
}

// Q2：Snapshot 的版本等於最後 Seq、等於重放的 State；
// 終態 Task 未載入也能 Snapshot；未知是 unknown_task。
func TestSnapshot(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, id)
	last := w.next(task.TurnCompletedKind)
	st, err := env.h.Snapshot(ctxb(), LocalOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	// 同一 State：重放 Log 結果一致
	recs, err := env.store.Read(ctxb(), id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	folded := task.State{ID: id}
	for _, r := range recs {
		e, err := task.DecodeEvent(r.Seq, r.Kind, r.At, r.Payload)
		if err != nil {
			t.Fatal(err)
		}
		folded = task.Evolve(folded, e)
	}
	if st.Version != folded.Version || st.Version != last.Seq+1 {
		t.Fatalf("version %d vs folded %d vs seq %d",
			st.Version, folded.Version, last.Seq)
	}
	if st.Title != folded.Title || st.Status != folded.Status ||
		len(st.Sessions) != len(folded.Sessions) {
		t.Fatalf("snapshot %+v vs folded %+v", st, folded)
	}
	// 終態 Task：關閉所有 session 後 Close/Start，actor 不載入
	if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdArchive}); err != nil {
		t.Fatal(err)
	}
	env.h.Close()
	h2 := env.restart(t)
	st2, err := h2.Snapshot(ctxb(), LocalOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Version != st.Version+1 || !st2.Archived {
		t.Fatalf("snapshot %+v", st2)
	}
	if _, err := h2.Snapshot(ctxb(), LocalOwner, "t-nope"); err == nil {
		t.Fatal("unknown accepted")
	} else {
		var he *Error
		if !errors.As(err, &he) || he.Code != CodeUnknownTask {
			t.Fatalf("got %v", err)
		}
	}
}

// Q3：兩個 Task 各一個遠端 Merge → 兩列；確認一筆 → 一列。
func TestConfirmationsAPI(t *testing.T) {
	env := newTestHub(t, nil)
	var dirs []string
	for i := range 2 {
		dir := gitRepo(t)
		dirs = append(dirs, dir)
		env.store.PutWorkspace(ctxb(), Workspace{
			ID: task.WorkspaceID(fmt.Sprintf("w-%d", i)), Path: realPath(t, dir),
			Policy: task.PolicyAlways})
	}
	if err := env.h.loadCache(ctxb()); err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		env.create(t, dir, "", testLaunch())
	}
	for _, id := range streamIDs(t, env) {
		if _, err := env.h.Command(ctxb(), Principal{Device: "d9"}, id, task.Command{
			Kind:  task.CmdMerge,
			Merge: &task.Merge{Target: task.Target{Level: task.TargetWorktree}}}); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := env.h.Confirmations(ctxb(), LocalOwner)
	if err != nil || len(cs) != 2 {
		t.Fatalf("confirmations %v %v", cs, err)
	}
	if _, err := env.h.Command(ctxb(), LocalOwner, cs[0].Task, task.Command{
		Kind:    task.CmdConfirm,
		Confirm: &task.ConfirmationRef{Confirmation: cs[0].ID}}); err != nil {
		t.Fatal(err)
	}
	cs, _ = env.h.Confirmations(ctxb(), LocalOwner)
	if len(cs) != 1 {
		t.Fatalf("confirmations %v", cs)
	}
}

func streamIDs(t *testing.T, env *testEnv) []task.TaskID {
	t.Helper()
	ids, err := env.store.Streams(ctxb())
	if err != nil || len(ids) == 0 {
		t.Fatal("no streams")
	}
	return ids
}

// Q4：全文檢索跨 Task，遮蔽後消失。
func TestSearchAPI(t *testing.T) {
	env := newTestHub(t, nil)
	id1 := env.create(t, t.TempDir(), "say hi", testLaunch())
	id2 := env.create(t, t.TempDir(), "say hi", testLaunch())
	w1, w2 := watch(t, env.h, id1), watch(t, env.h, id2)
	e1 := w1.next(task.MessageAppendedKind)
	m2 := w2.next(task.MessageAppendedKind)
	w1.next(task.TurnCompletedKind)
	w2.next(task.TurnCompletedKind)
	hits, err := env.h.Search(ctxb(), LocalOwner, "hi", 10)
	if err != nil {
		t.Fatal(err)
	}
	// brief "say hi" 與訊息 "hi" 各自一列；訊息列的 Seq 是事件的。
	has := func(id task.TaskID, seq task.Seq) bool {
		for _, h := range hits {
			if h.Task == id && h.Seq == seq {
				return true
			}
		}
		return false
	}
	if !has(id1, e1.Seq) {
		t.Fatalf("missing {t1,%d} in %v", e1.Seq, hits)
	}
	if _, err := env.h.Command(ctxb(), LocalOwner, id1, task.Command{
		Kind: task.CmdRedactMessage, Session: e1.Session,
		RedactMessage: &task.RedactMessage{Message: e1.MessageAppended.Message}}); err != nil {
		t.Fatal(err)
	}
	hits, _ = env.h.Search(ctxb(), LocalOwner, "hi", 10)
	for _, h := range hits {
		if h.Task == id1 && h.Seq == e1.Seq {
			t.Fatalf("redacted hit still in %v", hits)
		}
	}
	if !has(id2, m2.Seq) {
		t.Fatalf("missing {t2,%d} in %v", m2.Seq, hits)
	}
}

// Q5：workspace 規則與自動登記的 Workspace 立即可見。
func TestReadConfigVisible(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	id := env.create(t, dir, "ask a1 rm -rf /", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	e := w.next(task.PermissionAskedKind)
	if _, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{
			Permission: e.PermissionAsked.ID, Verdict: task.Allow,
			Always: &task.Rule{Scope: task.ScopeWorkspace,
				Match: task.MatchPrefix, Pattern: "rm"}}}); err != nil {
		t.Fatal(err)
	}
	rules, err := env.h.Rules(ctxb(), LocalOwner)
	if err != nil || len(rules) != 1 || rules[0].Pattern != "rm" {
		t.Fatalf("rules %v %v", rules, err)
	}
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	if len(ws) != 1 || ws[0].Path != realPath(t, dir) {
		t.Fatalf("workspaces %v", ws)
	}
}

// Q6：關閉後所有讀取都回 closed。
func TestReadsAfterClose(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	env.h.Close()
	checks := []error{}
	_, e := env.h.Tasks(ctxb(), LocalOwner)
	checks = append(checks, e)
	_, e = env.h.Snapshot(ctxb(), LocalOwner, id)
	checks = append(checks, e)
	_, e = env.h.Confirmations(ctxb(), LocalOwner)
	checks = append(checks, e)
	_, e = env.h.Search(ctxb(), LocalOwner, "x", 1)
	checks = append(checks, e)
	_, e = env.h.Workspaces(ctxb(), LocalOwner)
	checks = append(checks, e)
	_, e = env.h.Rules(ctxb(), LocalOwner)
	checks = append(checks, e)
	_, e = env.h.Settings(ctxb(), LocalOwner)
	checks = append(checks, e)
	for i, err := range checks {
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("check %d: %v", i, err)
		}
	}
}

// G1：改名+改策略生效，Path 不變；新 Task 用新策略。
func TestPutWorkspaceAPI(t *testing.T) {
	env := newTestHub(t, nil)
	dir := gitRepo(t)
	id := env.create(t, dir, "hang", testLaunch())
	w := watch(t, env.h, id)
	w.next(task.NativeSessionBoundKind) // Task 在跑
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	w0 := ws[0]
	w0.Name, w0.Policy = "renamed", task.PolicyNever
	w0.Path = "/elsewhere" // 應被忽略
	if err := env.h.PutWorkspace(ctxb(), LocalOwner, w0); err != nil {
		t.Fatal(err)
	}
	ws2, _ := env.h.Workspaces(ctxb(), LocalOwner)
	if ws2[0].Name != "renamed" || ws2[0].Policy != task.PolicyNever ||
		ws2[0].Path != realPath(t, dir) {
		t.Fatalf("workspace %+v", ws2[0])
	}
	// 快取已更新：auto 在同工作樹有運行 Task 時會是 worktree，
	// never 仍然是 none。
	id2 := env.create(t, dir, "", testLaunch())
	wt2 := watch(t, env.h, id2)
	e := wt2.next(task.TaskCreatedKind)
	if e.TaskCreated.Isolation != task.IsolationNone {
		t.Fatalf("isolation %v", e.TaskCreated.Isolation)
	}
}

// G2：未知 ID / 壞 policy / 空 name。
func TestPutWorkspaceInvalid(t *testing.T) {
	env := newTestHub(t, nil)
	env.create(t, t.TempDir(), "", testLaunch())
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	w := ws[0]
	bad := w
	bad.ID = "w-nope"
	if err := env.h.PutWorkspace(ctxb(), LocalOwner, bad); err == nil {
		t.Fatal("unknown accepted")
	} else {
		var he *Error
		if !errors.As(err, &he) || he.Code != CodeUnknownWorkspace {
			t.Fatalf("got %v", err)
		}
	}
	bad = w
	bad.Name, bad.Policy = "x", "bogus"
	err := env.h.PutWorkspace(ctxb(), LocalOwner, bad)
	assertField(t, err, "policy")
	bad = w
	bad.Name, bad.Policy = "", task.PolicyAuto
	assertField(t, env.h.PutWorkspace(ctxb(), LocalOwner, bad), "name")
}

// L2-hub：標題全是空白 → task.invalid，且不登記 Workspace。
func TestCreateTaskBlankTitle(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	_, _, err := env.h.CreateTask(ctxb(), LocalOwner, dir,
		task.CreateTask{Title: "   "})
	var rej *task.Rejection
	if !errors.As(err, &rej) || rej.Reason != task.ReasonInvalid {
		t.Fatalf("got %v", err)
	}
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	if len(ws) != 0 {
		t.Fatalf("workspaces %v", ws)
	}
}

// L6-hub：說明全是空白 → 標籤檢索列只有標題（D64/D65）。
func TestCreateTaskBlankBrief(t *testing.T) {
	env := newTestHub(t, nil)
	id, _, err := env.h.CreateTask(ctxb(), LocalOwner, t.TempDir(),
		task.CreateTask{Title: "Alpha", Brief: "   "})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := env.store.ViewsOf(ctxb(), id)
	if len(v.Search) != 1 || v.Search[0].Text != "Alpha" {
		t.Fatalf("search %+v", v.Search)
	}
}

// Q09-hub：不給上限（limit <= 0）時最多 20 條，是搜尋面板顯示的條數（D67）；給了就照給的，
// 命中不夠時有幾條回幾條；留下的是最近更新的，截掉的是較早更新的 Task。
func TestSearchLimit(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	var ids []task.TaskID // 依建立先後，也就是更新時間舊到新
	for i := range 25 {
		id, _, err := env.h.CreateTask(ctxb(), LocalOwner, dir,
			task.CreateTask{Title: fmt.Sprintf("Alpha %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		env.clock.Advance(time.Minute)
	}
	for _, tc := range []struct {
		limit, want int
	}{{0, 20}, {-1, 20}, {5, 5}, {21, 21}, {30, 25}} {
		hits, err := env.h.Search(ctxb(), LocalOwner, "alpha", tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != tc.want {
			t.Errorf("limit %d: %d hits, want %d", tc.limit, len(hits), tc.want)
			continue
		}
		// 新到舊，整段是最近更新的那幾個：第一條是最後建的，最後一條是留下的裡最早建的。
		for i, h := range hits {
			if want := ids[len(ids)-1-i]; h.Task != want {
				t.Errorf("limit %d: hit %d is %s, want %s", tc.limit, i, h.Task, want)
				break
			}
		}
	}
}

// 上限的規則：不給時 20、最多 500（超過 500 條的命中在 TestSearchLimit 裡造不出來）。
func TestSearchLimitRule(t *testing.T) {
	for _, tc := range []struct{ limit, want int }{
		{0, 20}, {-1, 20}, {math.MinInt, 20}, {1, 1}, {20, 20}, {499, 499}, {500, 500}, {501, 500}, {math.MaxInt, 500},
	} {
		if got := searchLimit(tc.limit); got != tc.want {
			t.Errorf("searchLimit(%d) = %d, want %d", tc.limit, got, tc.want)
		}
	}
}

// L10：Workspace 名稱去首尾空白後保存。
func TestPutWorkspaceTrimsName(t *testing.T) {
	env := newTestHub(t, nil)
	env.create(t, t.TempDir(), "", testLaunch())
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	w := ws[0]
	w.Name = "  Web "
	if err := env.h.PutWorkspace(ctxb(), LocalOwner, w); err != nil {
		t.Fatal(err)
	}
	ws2, _ := env.h.Workspaces(ctxb(), LocalOwner)
	if ws2[0].Name != "Web" {
		t.Fatalf("name %q", ws2[0].Name)
	}
}

// L11：名稱正規化後為空或含換行 → invalid_field{name}，名稱不變。
func TestPutWorkspaceBadName(t *testing.T) {
	env := newTestHub(t, nil)
	env.create(t, t.TempDir(), "", testLaunch())
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	for _, name := range []string{"  ", "a\nb"} {
		bad := ws[0]
		bad.Name = name
		assertField(t, env.h.PutWorkspace(ctxb(), LocalOwner, bad), "name")
	}
	ws2, _ := env.h.Workspaces(ctxb(), LocalOwner)
	if ws2[0].Name != ws[0].Name {
		t.Fatalf("name changed to %q", ws2[0].Name)
	}
}

// L12：workspaceName 規則 + 自動登記的名稱。
func TestWorkspaceName(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ dir, want string }{
		{filepath.Join(root, " x "), "x"},
		{filepath.Join(root, "a\nb"), "a b"},
		{filepath.Join(root, "   "), ""}, // 檔名全空白 → 用整個路徑，非空
	} {
		got := workspaceName(tc.dir)
		if tc.want != "" && got != tc.want {
			t.Fatalf("%q → %q, want %q", tc.dir, got, tc.want)
		}
		if _, ok := task.Label(got); !ok {
			t.Fatalf("%q → %q not a label", tc.dir, got)
		}
	}
	// 真的在建 Task 時套用：資料夾名 " x " → Workspace 名 "x"。
	env := newTestHub(t, nil)
	dir := filepath.Join(root, " ws ")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	env.create(t, dir, "", testLaunch())
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	if len(ws) != 1 || ws[0].Name != "ws" {
		t.Fatalf("workspaces %+v", ws)
	}
}

func assertField(t *testing.T, err error, field string) {
	t.Helper()
	var he *Error
	if !errors.As(err, &he) || he.Code != CodeInvalidField || he.Params["field"] != field {
		t.Fatalf("want invalid_field %s, got %v", field, err)
	}
}

// G3：遠端不能寫，讀可以。
func TestRemoteWritesDenied(t *testing.T) {
	env := newTestHub(t, nil)
	env.create(t, t.TempDir(), "", testLaunch())
	remote := Principal{Device: "d9"}
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	for _, err := range []error{
		env.h.PutWorkspace(ctxb(), remote, ws[0]),
		env.h.DeleteRule(ctxb(), remote, "r-1"),
		env.h.PutSettings(ctxb(), remote, Settings{}),
	} {
		var he *Error
		if !errors.As(err, &he) || he.Code != CodeNeedsDesktop {
			t.Fatalf("got %v", err)
		}
	}
	if _, err := env.h.Workspaces(ctxb(), remote); err != nil {
		t.Fatal("remote read failed")
	}
	if _, err := env.h.Tasks(ctxb(), remote); err != nil {
		t.Fatal("remote read failed")
	}
}

// G4：刪規則 → 同類 ask 不再自動決定；未知 ID 是 unknown_rule。
func TestDeleteRuleAPI(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	id1 := env.create(t, dir, "ask a1 rm -rf /", testLaunch())
	w1 := watch(t, env.h, id1)
	sid := w1.next(task.SessionCreatedKind).Session
	e := w1.next(task.PermissionAskedKind)
	env.h.Command(ctxb(), LocalOwner, id1, task.Command{
		Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{
			Permission: e.PermissionAsked.ID, Verdict: task.Allow,
			Always: &task.Rule{Scope: task.ScopeWorkspace,
				Match: task.MatchPrefix, Pattern: "rm"}}})
	rules, _ := env.h.Rules(ctxb(), LocalOwner)
	if err := env.h.DeleteRule(ctxb(), LocalOwner, rules[0].ID); err != nil {
		t.Fatal(err)
	}
	w1.cancel()
	// 同目錄的 Task B：同樣的 ask 不該被規則決定
	id2 := env.create(t, dir, "ask b1 rm -rf /", testLaunch())
	w2 := watch(t, env.h, id2)
	w2.until(func(e task.Event) bool { return e.Kind == task.PermissionAskedKind })
	// PermissionDecided 不該來；等一個會來的事件做屏障
	st, err := env.h.Snapshot(ctxb(), LocalOwner, id2)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range st.Sessions {
		for _, a := range x.Pending {
			if a.Decision != nil {
				t.Fatalf("decided %+v", a)
			}
		}
	}
	if err := env.h.DeleteRule(ctxb(), LocalOwner, "r-nope"); err == nil {
		t.Fatal("unknown accepted")
	} else {
		var he *Error
		if !errors.As(err, &he) || he.Code != CodeUnknownRule {
			t.Fatalf("got %v", err)
		}
	}
}

// G5：設定來回；沒有存過時 Settings 是 DefaultSettings。
func TestPutSettingsAPI(t *testing.T) {
	env := newTestHub(t, nil)
	env.create(t, t.TempDir(), "", testLaunch())
	if st, _ := env.h.Settings(ctxb(), LocalOwner); st != DefaultSettings() {
		t.Fatalf("fresh settings %+v", st)
	}
	x := DefaultSettings()
	x.Notify.Completed = true
	if err := env.h.PutSettings(ctxb(), LocalOwner, x); err != nil {
		t.Fatal(err)
	}
	st, _ := env.h.Settings(ctxb(), LocalOwner)
	if !st.Notify.Completed {
		t.Fatalf("settings %+v", st)
	}
}

// G6：同一 workspace 再建 Task → LastUsedAt 更新。
func TestLastUsedAtTouch(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	env.create(t, dir, "", testLaunch())
	env.clock.Advance(2 * time.Hour)
	before := h_now(env)
	env.clock.Advance(time.Minute)
	env.create(t, dir, "", testLaunch())
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	if len(ws) != 1 {
		t.Fatalf("workspaces %v", ws)
	}
	if !ws[0].LastUsedAt.After(before) {
		t.Fatalf("last_used_at %v", ws[0].LastUsedAt)
	}
}

func h_now(env *testEnv) time.Time {
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	return ws[0].LastUsedAt
}

// K1：繞過 hub 直接改 store，快取仍生效；重啟後重載。
func TestCacheSoleWriter(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	id := env.create(t, dir, "ask a1 rm -rf /", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	e := w.next(task.PermissionAskedKind)
	env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{
			Permission: e.PermissionAsked.ID, Verdict: task.Allow,
			Always: &task.Rule{Scope: task.ScopeWorkspace,
				Match: task.MatchPrefix, Pattern: "rm"}}})
	rules, _ := env.h.Rules(ctxb(), LocalOwner)
	w.cancel()
	// 繞過 hub 刪掉：快取仍決定
	env.store.DeleteRule(ctxb(), rules[0].ID)
	id2 := env.create(t, dir, "ask b1 rm -rf /", testLaunch())
	w2 := watch(t, env.h, id2)
	e2 := w2.until(func(e task.Event) bool {
		return e.Kind == task.PermissionDecidedKind
	})
	if e2.PermissionDecided.Decision.Source != task.DecidedByRule {
		t.Fatalf("decision %+v", e2.PermissionDecided)
	}
	w2.cancel()
	env.h.Close()
	// 重啟：規則真的沒了
	h2 := env.restart(t)
	id3, _, err := h2.CreateTask(ctxb(), LocalOwner, dir, task.CreateTask{
		Title: "t3", Brief: "ask c1 rm -rf /", Launches: []task.LaunchSnapshot{testLaunch()}})
	if err != nil {
		t.Fatal(err)
	}
	w3 := watch(t, h2, id3)
	w3.until(func(e task.Event) bool { return e.Kind == task.PermissionAskedKind })
	st, _ := h2.Snapshot(ctxb(), LocalOwner, id3)
	for _, x := range st.Sessions {
		for _, a := range x.Pending {
			if a.Decision != nil {
				t.Fatalf("decided %+v", a)
			}
		}
	}
}

// K2：兩個併發 CreateTask 同一未登記目錄 → 只有一個 Workspace。
func TestCreateTaskRace(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	start := make(chan struct{})
	var wg sync.WaitGroup
	ids := make([]task.TaskID, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], _, errs[i] = env.h.CreateTask(ctxb(), LocalOwner, dir,
				task.CreateTask{Title: "t", Launches: []task.LaunchSnapshot{}})
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
	}
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	if len(ws) != 1 {
		t.Fatalf("workspaces %v", ws)
	}
	for _, id := range ids {
		st, err := env.h.Snapshot(ctxb(), LocalOwner, id)
		if err != nil {
			t.Fatal(err)
		}
		if st.Workspace != ws[0].ID {
			t.Fatalf("task %s ws %s", id, st.Workspace)
		}
	}
}

// --- 通知 ---

func nextNotify(t *testing.T, env *testEnv) Notification {
	t.Helper()
	select {
	case n := <-env.notify:
		return n
	case <-time.After(testWait):
		t.Fatal("no notification")
		return Notification{}
	}
}

func drainNotify(env *testEnv) []Notification {
	var out []Notification
	for {
		select {
		case n := <-env.notify:
			out = append(out, n)
		default:
			return out
		}
	}
}

// T1：未決定的 ask 通知一次；規則決定的不通知。
func TestNotifyAsk(t *testing.T) {
	env := newTestHub(t, nil)
	dir := t.TempDir()
	id := env.create(t, dir, "ask a1 rm -rf /", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	n := nextNotify(t, env)
	if n.Kind != NotifyAsk || n.Task != id || n.Session != sid ||
		n.Params["command"] != "rm -rf /" || n.Params["agent"] != "Test Agent" ||
		n.Params["tool"] != "Bash" || n.TaskTitle == "" {
		t.Fatalf("notify %+v", n)
	}
	e := w.next(task.PermissionAskedKind)
	// 寫入 workspace 規則（經 hub 快取），讓同目錄的下一個 ask 被規則決定。
	env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{
			Permission: e.PermissionAsked.ID, Verdict: task.Allow,
			Always: &task.Rule{Scope: task.ScopeWorkspace,
				Match: task.MatchPrefix, Pattern: "rm"}}})
	w.next(task.TurnCompletedKind)
	w.cancel()
	drainNotify(env)
	// Task B 同 workspace：ask 被規則決定 → 無 ask 通知；
	// 屏障是它自己的 TurnCompleted。
	id2 := env.create(t, dir, "ask b1 rm -rf /", testLaunch())
	w2 := watch(t, env.h, id2)
	w2.next(task.PermissionDecidedKind)
	w2.next(task.TurnCompletedKind)
	for _, n := range drainNotify(env) {
		if n.Kind == NotifyAsk {
			t.Fatalf("rule-decided ask notified %+v", n)
		}
	}
}

// T2：crash → failed，原因是 proc.exited，帶退出狀態。
func TestNotifyFailed(t *testing.T) {
	env := newTestHub(t, nil)
	env.create(t, t.TempDir(), "crash", testLaunch())
	var n Notification
	for range 8 {
		n = nextNotify(t, env)
		if n.Kind == NotifyFailed {
			break
		}
	}
	if n.Kind != NotifyFailed || n.Reason == nil || n.Reason.Code != "proc.exited" || n.Reason.Params["status"] == "" {
		t.Fatalf("notify %+v", n)
	}
}

// T3：遠端 Merge → confirmation 通知 kind=merge。
func TestNotifyConfirmation(t *testing.T) {
	env := newTestHub(t, nil)
	dir := gitRepo(t)
	env.store.PutWorkspace(ctxb(), Workspace{
		ID: "w-c", Path: realPath(t, dir), Policy: task.PolicyAlways})
	if err := env.h.loadCache(ctxb()); err != nil {
		t.Fatal(err)
	}
	env.create(t, dir, "", testLaunch())
	if _, err := env.h.Command(ctxb(), Principal{Device: "d9"}, streamIDs(t, env)[0], task.Command{
		Kind:  task.CmdMerge,
		Merge: &task.Merge{Target: task.Target{Level: task.TargetWorktree}}}); err != nil {
		t.Fatal(err)
	}
	var n Notification
	for range 8 {
		n = nextNotify(t, env)
		if n.Kind == NotifyConfirmation {
			break
		}
	}
	if n.Kind != NotifyConfirmation || n.Params["kind"] != "merge" {
		t.Fatalf("notify %+v", n)
	}
}

// T4：completed 預設關閉；打開後有。
func TestNotifyCompletedDefault(t *testing.T) {
	env := newTestHub(t, nil)
	env.create(t, t.TempDir(), "say hi", testLaunch())
	// 屏障：等 watcher 看到 TurnCompleted，再斷言通知佇列裡沒有 completed
	w := watch(t, env.h, env.h.actors[streamIDs(t, env)[0]].id)
	_ = w
	id := streamIDs(t, env)[0]
	w2 := watch(t, env.h, id)
	w2.next(task.TurnCompletedKind)
	// 讓通知 goroutine 有機會送達：下一個命令往返當屏障
	env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "t2", Brief: "b"}})
	for _, n := range drainNotify(env) {
		if n.Kind == NotifyCompleted {
			t.Fatal("completed notified by default")
		}
	}
	x := DefaultSettings()
	x.Notify.Completed = true
	env.h.PutSettings(ctxb(), LocalOwner, x)
	env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdSubmit, Session: st2sid(t, env, id),
		Submit: &task.Submit{Prompt: task.Prompt{Text: "say bye"}}})
	var n Notification
	for range 8 {
		n = nextNotify(t, env)
		if n.Kind == NotifyCompleted {
			break
		}
	}
	if n.Kind != NotifyCompleted {
		t.Fatal("no completed after enabling")
	}
}

func st2sid(t *testing.T, env *testEnv, id task.TaskID) task.SessionID {
	t.Helper()
	st, err := env.h.Snapshot(ctxb(), LocalOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	for sid := range st.Sessions {
		return sid
	}
	t.Fatal("no session")
	return ""
}

// T5：Notify.ask=false → ask 不通知；屏障是 deny 後的 failed 通知。
func TestNotifyFiltered(t *testing.T) {
	env := newTestHub(t, nil)
	x := DefaultSettings()
	x.Notify.Ask = false
	env.h.PutSettings(ctxb(), LocalOwner, x)
	id := env.create(t, t.TempDir(), "ask a1 rm -rf /", testLaunch())
	w := watch(t, env.h, id)
	sid := w.next(task.SessionCreatedKind).Session
	e := w.next(task.PermissionAskedKind)
	// deny → 回合失敗 → failed 通知是 FIFO 裡 ask 之後的屏障
	env.h.Command(ctxb(), LocalOwner, id, task.Command{
		Kind: task.CmdDecidePermission, Session: sid,
		DecidePermission: &task.DecidePermission{
			Permission: e.PermissionAsked.ID, Verdict: task.Deny}})
	var sawFailed bool
	for range 8 {
		n := nextNotify(t, env)
		if n.Kind == NotifyAsk {
			t.Fatal("ask notified despite filter")
		}
		if n.Kind == NotifyFailed {
			sawFailed = true
			break
		}
	}
	if !sawFailed {
		t.Fatal("no failed notification")
	}
}

// T6：Notifier nil 或回錯 → hub 照常。
func TestNotifyBroken(t *testing.T) {
	env := newTestHub(t, func(o *Options) {
		o.Notifier = chanNotifier{ch: make(chan Notification), err: errors.New("x")}
	})
	env.create(t, t.TempDir(), "say hi", testLaunch())
	w := watch(t, env.h, streamIDs(t, env)[0])
	w.next(task.TurnCompletedKind)
	env2 := newTestHub(t, func(o *Options) { o.Notifier = nil })
	env2.create(t, t.TempDir(), "say hi", testLaunch())
	w2 := watch(t, env2.h, streamIDs(t, env2)[0])
	w2.next(task.TurnCompletedKind)
}

// T7：堵到 ctx 的 Notifier → Close 正常返回。
func TestNotifyBlockedClose(t *testing.T) {
	env := newTestHub(t, func(o *Options) {
		o.Notifier = NotifierFunc(func(ctx context.Context, n Notification) error {
			<-ctx.Done()
			return ctx.Err()
		})
	})
	env.create(t, t.TempDir(), "say hi", testLaunch())
	done := make(chan struct{})
	go func() { env.h.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatal("Close blocked by notifier")
	}
}

// NotifierFunc 讓函數當 Notifier。
type NotifierFunc func(context.Context, Notification) error

func (f NotifierFunc) Notify(ctx context.Context, n Notification) error {
	return f(ctx, n)
}

// T8：notifications() 純函數表格測試。
func TestNotificationsPure(t *testing.T) {
	s := task.State{ID: "t-1", Title: "title"}
	ask := task.PermissionAskedKind
	_ = ask
	// ForkRecommended
	evs := []task.Event{{
		Kind: task.ForkRecommendedKind, Session: "s-1",
		ForkRecommended: &task.Recommendation{Winner: "s-9"}}}
	got := notifications(s, evs)
	if len(got) != 1 || got[0].Kind != NotifyRecommended ||
		got[0].Params["winner"] != "s-9" || got[0].TaskTitle != "title" {
		t.Fatalf("%+v", got)
	}
	// MergeConflicted：files 數
	evs = []task.Event{{
		Kind:            task.MergeConflictedKind,
		MergeConflicted: &task.MergeConflict{Files: []string{"a", "b", "c"}}}}
	got = notifications(s, evs)
	if len(got) != 1 || got[0].Kind != NotifyConflict || got[0].Params["files"] != "3" {
		t.Fatalf("%+v", got)
	}
	// 同批 PermissionAsked + PermissionDecided（rule）→ 無 ask
	askID := task.PermissionID("p-1")
	base := task.Event{Kind: task.SessionCreatedKind, Session: "s-1",
		SessionCreated: &task.SessionCreated{
			Launch: task.LaunchSnapshot{Agent: "test"}}}
	askEv := task.Event{Kind: task.PermissionAskedKind, Session: "s-1",
		PermissionAsked: &task.Ask{ID: askID,
			Request: task.PermissionRequest{Tool: "Bash", Command: "rm x"}}}
	st := s
	st = task.Evolve(st, base)
	// 未決定的 ask → 通知，參數齊全
	st1 := task.Evolve(st, askEv)
	got = notifications(st1, []task.Event{askEv})
	if len(got) != 1 || got[0].Kind != NotifyAsk ||
		got[0].Params["command"] != "rm x" || got[0].Params["agent"] != "test" {
		t.Fatalf("%+v", got)
	}
	// 同批決定的 → 無
	decEv := task.Event{Kind: task.PermissionDecidedKind, Session: "s-1",
		PermissionDecided: &task.PermissionDecided{Permission: askID,
			Decision: task.Decision{Verdict: task.Allow, Source: task.DecidedByRule}}}
	st2 := task.Evolve(task.Evolve(st, askEv), decEv)
	if got := notifications(st2, []task.Event{askEv, decEv}); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	// Settings.Notifies 預設；未知的種類不發
	z := DefaultSettings()
	for k, want := range map[NotificationKind]bool{
		NotifyAsk: true, NotifyFailed: true, NotifyStalled: true, NotifyRecommended: true,
		NotifyConfirmation: true, NotifyConflict: true, NotifyCompleted: false} {
		if z.Notifies(k) != want {
			t.Fatalf("%s default %v", k, z.Notifies(k))
		}
	}
	if z.Notifies("bogus") {
		t.Fatal("unknown kind notified")
	}
}

// 遠端對未登記、不存在的目錄建 Task → unknown_workspace，
// 不經 Git.Inspect（不透露路徑存在與否）。
func TestRemoteCreateUnregisteredDir(t *testing.T) {
	env := newTestHub(t, nil)
	_, _, err := env.h.CreateTask(ctxb(), Principal{Device: "d9"},
		t.TempDir()+"/nope", task.CreateTask{Title: "t"})
	var he *Error
	if !errors.As(err, &he) || he.Code != CodeUnknownWorkspace {
		t.Fatalf("got %v", err)
	}
}

// Snapshot 帶已取消的 ctx → context.Canceled。
func TestSnapshotCancelledCtx(t *testing.T) {
	env := newTestHub(t, nil)
	id := env.create(t, t.TempDir(), "", testLaunch())
	ctx, cancel := context.WithCancel(ctxb())
	cancel()
	_, err := env.h.Snapshot(ctx, LocalOwner, id)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
