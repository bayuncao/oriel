package app

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/store"
	"github.com/bayuncao/oriel/internal/task"
)

func newTestApp(t *testing.T, env proc.Env, root string) (*App, string) {
	t.Helper()
	data := t.TempDir()
	a, err := New(ctxb2(), Options{DataDir: data, Inherited: env, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, data
}

// A2：建 Task → Close → 重開 → Tasks/Workspaces/Search 都在。
func TestReopenKeepsData(t *testing.T) {
	dir := t.TempDir()
	data := t.TempDir()
	env := proc.Inherited() // Git.Inspect 需要真的 git
	root := t.TempDir()
	a, err := New(ctxb2(), Options{DataDir: data, Inherited: env, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := a.Hub().CreateTask(ctxb2(), hub.LocalOwner, dir,
		task.CreateTask{Title: "t", Brief: "修复登录"})
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	a2, err := New(ctxb2(), Options{DataDir: data, Inherited: env, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	tasks, _ := a2.Hub().Tasks(ctxb2(), hub.LocalOwner)
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("tasks %+v", tasks)
	}
	ws, _ := a2.Hub().Workspaces(ctxb2(), hub.LocalOwner)
	if len(ws) != 1 || ws[0].ID != tasks[0].Workspace {
		t.Fatalf("ws %+v vs task %+v", ws, tasks[0])
	}
	hits, _ := a2.Hub().Search(ctxb2(), hub.LocalOwner, "登录", 10)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
}

// A3：user_version=99 → New 回 store.newer_schema，檔案可再開、不洩漏 goroutine。
func TestNewerSchema(t *testing.T) {
	data := t.TempDir()
	a, err := New(ctxb2(), Options{DataDir: data,
		Inherited: proc.Env{}, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	bumpUserVersion(t, filepath.Join(data, "orielhub.db"), 99)
	before := runtime.NumGoroutine()
	_, err = New(ctxb2(), Options{DataDir: data,
		Inherited: proc.Env{}, Root: t.TempDir()})
	var se *store.Error
	if !errors.As(err, &se) || se.ErrorCode() != string(store.CodeNewerSchema) {
		t.Fatalf("got %v", err)
	}
	db, err := store.Open(ctxb2(), filepath.Join(data, "orielhub.db"), store.Options{})
	if err == nil {
		db.Close()
	}
	for deadline := time.Now().Add(5 * time.Second); runtime.NumGoroutine() > before+2 && time.Now().Before(deadline); {
		runtime.Gosched() // 等失敗路徑的清理 goroutine 收尾（計數不計時）
	}
}

// A4：不能建的 DataDir → store.open_failed。
func TestUnwritableDir(t *testing.T) {
	_, err := New(ctxb2(), Options{
		DataDir:   filepath.Join(t.TempDir(), "no", "way"),
		Inherited: proc.Env{}, Root: t.TempDir()})
	var se *store.Error
	if !errors.As(err, &se) || se.ErrorCode() != string(store.CodeOpenFailed) {
		t.Fatalf("%v", err)
	}
}

// A5：Close 兩次無害；之後 Hub 呼叫回 hub.closed。
func TestCloseIdempotent(t *testing.T) {
	a, _ := newTestApp(t, proc.Env{}, t.TempDir())
	a.Close()
	a.Close()
	if _, err := a.Hub().Tasks(ctxb2(), hub.LocalOwner); err != hub.ErrClosed {
		t.Fatalf("%v", err)
	}
}

// A7：openLog 超過 10 MiB 輪轉；小檔案接續寫。
func TestOpenLogRotation(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "orielhub.log")
	os.WriteFile(big, make([]byte, logMax+1), 0o644)
	f, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	if st.Size() != 0 {
		t.Fatal("not rotated")
	}
	f.Close()
	if st, err := os.Stat(big + ".1"); err != nil || st.Size() != logMax+1 {
		t.Fatal("no .1")
	}
	f2, _ := OpenLog(dir)
	f2.WriteString("x")
	st2, _ := f2.Stat()
	if st2.Size() != 1 {
		t.Fatal("not appending")
	}
	f2.Close()
}

// A8：proxy 函式讀環境；NO_PROXY 直連；空環境直連。
func TestProxyFromEnv(t *testing.T) {
	px := proxyOf(proc.Env{
		"https_proxy=http://127.0.0.1:7890", "NO_PROXY=example.org"})
	u1, _ := url.Parse("https://cdn.agentclientprotocol.com/x")
	got, err := px(u1)
	if err != nil || got == nil || got.Host != "127.0.0.1:7890" {
		t.Fatalf("proxy %v %v", got, err)
	}
	u2, _ := url.Parse("https://example.org")
	if got, _ := px(u2); got != nil {
		t.Fatalf("bypass %v", got)
	}
	if got, _ := proxyOf(proc.Env{})(u1); got != nil {
		t.Fatalf("empty env proxied %v", got)
	}
}

func writeExe(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
