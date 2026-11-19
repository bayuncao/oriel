package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bayuncao/oriel/internal/task"
)

// P1/P2：符號連結與真實路徑登記的是同一個 Workspace（D72）。
func TestCreateTaskResolvesSymlinks(t *testing.T) {
	env := newTestHub(t, nil)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// P1：經 link 建 → 登記的是真實路徑，名稱取真實路徑最後一段。
	env.create(t, link, "", testLaunch())
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	want := realPath(t, real)
	if len(ws) != 1 || ws[0].Path != want || ws[0].Name != filepath.Base(want) {
		t.Fatalf("workspaces %+v", ws)
	}
	// P2：真實路徑與 link 都命中同一個 Workspace。
	for _, d := range []string{real, link} {
		env.create(t, d, "", testLaunch())
	}
	ws, _ = env.h.Workspaces(ctxb(), LocalOwner)
	if len(ws) != 1 {
		t.Fatalf("workspaces %+v", ws)
	}
}

// P4：遠端只做字串比對、不碰檔案系統；link 路徑不算登記（D72）。
func TestRemoteCreateExactMatchOnly(t *testing.T) {
	env := newTestHub(t, nil)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	env.create(t, real, "", testLaunch()) // 桌面登記真實路徑
	remote := Principal{Device: "d9"}
	// link 不是登記路徑 → unknown_workspace。
	if _, _, err := env.h.CreateTask(ctxb(), remote, link,
		task.CreateTask{Title: "t"}); err == nil {
		t.Fatal("link accepted")
	} else {
		var he *Error
		if !errors.As(err, &he) || he.Code != CodeUnknownWorkspace {
			t.Fatalf("got %v", err)
		}
	}
	// 精確的登記路徑可以用。
	if _, _, err := env.h.CreateTask(ctxb(), remote, realPath(t, real),
		task.CreateTask{Title: "t"}); err != nil {
		t.Fatalf("registered path: %v", err)
	}
}

// P6–P11：OtherTasksRunning 依工作樹根包含判斷（D72）。M4 之前它不影響隔離
// 方式（D97），這裡直接核對 hub 算出的值。
func TestOtherRunningByWorktreeRoot(t *testing.T) {
	repo := gitRepo(t)
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(repo, "a"), filepath.Join(repo, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(repo, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRepoIn(t, nested)
	other := gitRepo(t)

	for _, tc := range []struct {
		name           string
		runDir, newDir string
		want           bool
	}{
		{"P6 parent running, create sub", repo, sub, true},
		{"P7 sibling running, create sibling", a, b, true},
		{"P8 sub running, create parent", sub, repo, true},
		{"P9 other repo", other, repo, false},
		{"P10 nested repo running, create outer", nested, repo, true},
		{"P11 outer running, create nested", repo, nested, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestHub(t, nil)
			id := env.create(t, tc.runDir, "hang", testLaunch())
			w := watch(t, env.h, id)
			w.next(task.NativeSessionBoundKind)
			r, err := env.h.git.Inspect(ctxb(), tc.newDir)
			if err != nil {
				t.Fatal(err)
			}
			if got := env.h.otherRunning(r.Root, nil); got != tc.want {
				t.Fatalf("other running %v, want %v", got, tc.want)
			}
		})
	}
}

// W06、W08（D71）：auto、always 與 never 都能存，存了讀回來是它；其他值是 hub.invalid_field{policy}，不改策略。
func TestPutWorkspacePolicy(t *testing.T) {
	env := newTestHub(t, nil)
	env.create(t, t.TempDir(), "", testLaunch())
	ws, _ := env.h.Workspaces(ctxb(), LocalOwner)
	for _, pol := range []task.IsolationPolicy{"bogus", ""} {
		bad := ws[0]
		bad.Policy = pol
		assertField(t, env.h.PutWorkspace(ctxb(), LocalOwner, bad), "policy")
	}
	ws2, _ := env.h.Workspaces(ctxb(), LocalOwner)
	if ws2[0].Policy != ws[0].Policy {
		t.Fatalf("policy changed to %v", ws2[0].Policy)
	}
	for _, pol := range []task.IsolationPolicy{task.PolicyAlways, task.PolicyNever, task.PolicyAuto} {
		ok := ws[0]
		ok.Policy = pol
		if err := env.h.PutWorkspace(ctxb(), LocalOwner, ok); err != nil {
			t.Fatalf("%v: %v", pol, err)
		}
		got, _ := env.h.Workspaces(ctxb(), LocalOwner)
		if got[0].Policy != pol {
			t.Fatalf("policy %v after putting %v", got[0].Policy, pol)
		}
	}
	_ = context.Background
}
