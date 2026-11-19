//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
)

// newDiscovery 回傳一個在 m 上、登入 shell 為 shell 的
// Discovery，以及一個發送它所發佈表格的 channel。
func (m *machine) newDiscovery(shell string, defs ...Definition) (*Discovery, <-chan []Installation) {
	d := NewDiscovery(DiscoveryOptions{
		Runner:    &proc.Runner{},
		Root:      m.root,
		Inherited: proc.Env{"HOME=" + m.home, "SHELL=" + shell, "PATH=/usr/bin:/bin"},
	})
	// 只有測試預算本身的測試才設短的預算；其餘的一律等快照，不受負載影響。
	d.budget = time.Hour
	d.det.run = withoutDeadline(d.det.runner)
	d.SetAgents(defs, nil)
	m.t.Cleanup(d.Close)
	return d, d.Changes()
}

func next(t *testing.T, c <-chan []Installation) []Installation {
	t.Helper()
	select {
	case tab := <-c:
		return tab
	case <-time.After(30 * time.Second):
		t.Fatal("no table published")
		return nil
	}
}

// loginShell 寫入一個假的登入 shell，在 gate 不為空時
// 先等待 gate 檔案存在，再將 dir 前置到 PATH。
func (m *machine) loginShell(dir, gate string) string {
	wait := ""
	if gate != "" {
		wait = "read _ < '" + gate + "'\n"
	}
	return m.script(filepath.Join(m.root, "zsh"), wait+"export PATH='"+dir+"':$PATH\nshift\nexec /bin/sh -c \"$1\"")
}

func TestDiscoveryUsesSnapshot(t *testing.T) {
	m := newMachine(t)
	foo := m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery(m.loginShell(m.path, ""), fooDef())
	d.Start()
	tab := next(t, changes)
	if len(tab) != 1 || tab[0].Availability != Ready || tab[0].Path != foo {
		t.Fatalf("got %+v", tab)
	}
	env, err := d.Env(context.Background())
	if err != nil || env.PathDirs()[0] != m.path {
		t.Fatalf("Env = %v, %v", env, err)
	}
}

func TestDiscoveryLateSnapshotRescans(t *testing.T) {
	m := newMachine(t)
	gate := m.fifo("gate")
	foo := m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery(m.loginShell(m.path, gate), fooDef())
	d.budget = 50 * time.Millisecond
	d.Start()

	// 預算用完：第一份表格使用 fallback 環境，
	// 其中 foo 不在 PATH 上。
	if tab := next(t, changes); tab[0].Availability != Missing {
		t.Fatalf("first table: %+v", tab)
	}
	release(t, gate)
	if tab := next(t, changes); tab[0].Availability != Ready || tab[0].Path != foo {
		t.Fatalf("after the snapshot: %+v", tab)
	}
}

func TestDiscoveryFallbackHasToolDirs(t *testing.T) {
	m := newMachine(t)
	// 沒有 shell 回應；工具目錄中的程式仍會被找到。
	tool := m.script(filepath.Join(m.home, ".local/bin/foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery(m.script(filepath.Join(m.root, "zsh"), "exit 1"), fooDef())
	d.Start()
	if tab := next(t, changes); tab[0].Availability != Ready || tab[0].Path != tool {
		t.Fatalf("got %+v", tab)
	}
}

func TestDiscoveryVerify(t *testing.T) {
	m := newMachine(t)
	foo := m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery(m.loginShell(m.path, ""), fooDef())
	d.Start()
	next(t, changes)

	in, err := d.Verify(context.Background(), "foo")
	if err != nil || in.Version != "1.0.0" || runs(foo) != 1 {
		t.Fatalf("unchanged: %+v, %v, runs %d", in, err, runs(foo))
	}
	m.script(foo, "echo foo 1.1.0 # updated")
	if in, err = d.Verify(context.Background(), "foo"); err != nil || in.Version != "1.1.0" {
		t.Fatalf("changed: %+v, %v", in, err)
	}
	if _, err := d.Verify(context.Background(), "nope"); err != ErrUnknownAgent {
		t.Fatalf("unknown agent: %v", err)
	}
}

func TestDiscoveryRetriesTimeoutOnce(t *testing.T) {
	m := newMachine(t)
	// 第一次探測逾時；之後的探測真的執行。
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery(m.loginShell(m.path, ""), fooDef())
	var first atomic.Bool
	d.det.run = func(ctx context.Context, p proc.Probe) proc.Result {
		if first.CompareAndSwap(false, true) {
			return timedOutResult
		}
		return withoutDeadline(d.det.runner)(ctx, p)
	}
	d.Start()
	if tab := next(t, changes); tab[0].Availability != Broken {
		t.Fatalf("first: %+v", tab)
	}
	if tab := next(t, changes); tab[0].Availability != Ready {
		t.Fatalf("retry: %+v", tab)
	}
}

func TestDiscoveryRefresh(t *testing.T) {
	m := newMachine(t)
	d, changes := m.newDiscovery(m.loginShell(m.path, ""), fooDef())
	d.Start()
	if tab := next(t, changes); tab[0].Availability != Missing {
		t.Fatalf("first: %+v", tab)
	}
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tab := d.Installations(); tab[0].Availability != Ready {
		t.Fatalf("after refresh: %+v", tab)
	}
}

func TestDiscoveryAfterClose(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery(m.loginShell(m.path, ""), fooDef())
	d.Start()
	next(t, changes)
	d.Close()
	// 這些都不得在已關閉的 Discovery 上啟動工作或 panic。
	d.SetAgents([]Definition{fooDef()}, nil)
	d.Start()
	if err := d.Refresh(context.Background()); err != ErrClosed {
		t.Fatalf("Refresh after Close: %v", err)
	}
	d.Close()
}

func TestDiscoveryEnvBeforeStartTakesSnapshot(t *testing.T) {
	m := newMachine(t)
	d, _ := m.newDiscovery(m.loginShell(m.path, ""), fooDef())
	// 建立 Session 可能早於 Start：仍必須等快照，而不是拿 fallback。
	env, err := d.Env(context.Background())
	if err != nil || env.PathDirs()[0] != m.path {
		t.Fatalf("Env before Start = %v, %v", env.PathDirs(), err)
	}
}

func TestDiscoveryEnvAfterClose(t *testing.T) {
	m := newMachine(t)
	d, _ := m.newDiscovery(m.loginShell(m.path, ""), fooDef())
	d.Close()
	if _, err := d.Env(context.Background()); err != ErrClosed {
		t.Fatalf("Env after Close: %v", err)
	}
}

// snapshotCall 是一次假的快照請求，由測試決定何時以哪個環境回答。
type snapshotCall struct {
	ctx    context.Context
	answer chan proc.Env
}

// fakeSnapshots 以測試控制的假快照取代詢問 shell。
func fakeSnapshots(d *Discovery) <-chan snapshotCall {
	calls := make(chan snapshotCall, 8)
	d.takeEnv = func(ctx context.Context, _ proc.Env) (proc.Env, error) {
		c := snapshotCall{ctx: ctx, answer: make(chan proc.Env)}
		calls <- c
		select {
		case env := <-c.answer:
			return env, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return calls
}

func recv[T any](t *testing.T, c <-chan T) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(30 * time.Second):
		t.Fatal("nothing received")
		panic("unreachable")
	}
}

// fifo 建立具名管道：腳本讀它（read _ < fifo）會阻塞到測試 release，
// 寫它（echo > fifo）會阻塞到測試 await。
func (m *machine) fifo(name string) string {
	m.t.Helper()
	p := filepath.Join(m.root, name)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		m.t.Fatal(err)
	}
	return p
}

// release 讓一個讀 fifo 的腳本繼續；await 等一個腳本寫 fifo。
// 30 秒只是把卡住變成失敗的保險。
func release(t *testing.T, fifo string) {
	t.Helper()
	pipe(t, fifo, os.O_WRONLY)
}

func await(t *testing.T, fifo string) {
	t.Helper()
	pipe(t, fifo, os.O_RDONLY)
}

func pipe(t *testing.T, fifo string, flag int) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifo, flag, 0)
		if err == nil {
			if flag == os.O_WRONLY {
				_, err = f.Write([]byte("\n"))
			} else {
				_, err = f.Read(make([]byte, 1))
			}
			f.Close()
		}
		done <- err
	}()
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryRefreshThenClose(t *testing.T) {
	m := newMachine(t)
	foo := m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	errc := make(chan error, 1)
	go func() { errc <- d.Refresh(context.Background()) }()
	recv(t, calls) // 快照進行中
	d.Close()
	if err := recv(t, errc); err != ErrClosed {
		t.Fatalf("Refresh during Close: %v", err)
	}
	if runs(foo) != 0 {
		t.Fatalf("foo ran %d times after Close", runs(foo))
	}
	// Close 關閉了 Changes，而且之前沒有送出任何表格。
	if tab, ok := <-changes; ok {
		t.Fatalf("published: %+v", tab)
	}
}

func TestDiscoveryEnvFollowsSupersedingSnapshot(t *testing.T) {
	m := newMachine(t)
	d, _ := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	envc := make(chan proc.Env, 1)
	go func() {
		env, _ := d.Env(context.Background())
		envc <- env
	}()
	a := recv(t, calls)
	refreshed := make(chan error, 1)
	go func() { refreshed <- d.Refresh(context.Background()) }()
	b := recv(t, calls)
	// 被取代的快照被取消；等它的 Env 改等取代它的那一次。
	recv(t, a.ctx.Done())
	b.answer <- m.env
	if env := recv(t, envc); env.Get("PATH") != m.env.Get("PATH") {
		t.Fatalf("Env = %v", env)
	}
	if err := recv(t, refreshed); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryRescansOnceWithCurrentInputs(t *testing.T) {
	m := newMachine(t)
	started, gate := m.fifo("started"), m.fifo("gate")
	slow := m.script(filepath.Join(m.path, "foo"), "echo > '"+started+"'\nread _ < '"+gate+"'\necho foo 1.0.0")
	d, changes := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	d.Start()
	recv(t, calls).answer <- m.env
	await(t, started)

	// 舊掃描執行中，使用者連續改了五次設定，最後選了另一個可執行檔。
	other := m.script(filepath.Join(m.root, "opt/foo"), "echo foo 2.0.0")
	for range 5 {
		d.SetAgents([]Definition{fooDef()}, []Override{{Agent: "foo", Executable: other}})
	}
	release(t, gate)
	// 舊輸入的結果沒有寫入；多次變更合併成一次重掃。
	if tab := next(t, changes); len(tab) != 1 || tab[0].Path != other || tab[0].Version != "2.0.0" {
		t.Fatalf("first table: %+v", tab)
	}
	if runs(slow) != 1 || runs(other) != 1 {
		t.Fatalf("runs: stale %d, rescans %d", runs(slow), runs(other))
	}
}

func TestDiscoveryCloseFromReader(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	d.Start()
	recv(t, calls).answer <- m.env
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range changes {
			// 讀取者在處理表格時呼叫 Discovery，包括 Close，都不得死鎖。
			d.Installations()
			if _, err := d.Verify(context.Background(), "foo"); err != nil && err != ErrClosed {
				t.Errorf("Verify: %v", err)
			}
			d.Close()
		}
	}()
	recv(t, done) // Close 關閉了 Changes，讀取迴圈結束
}

func TestDiscoveryNoResidentGoroutines(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	d.Start()
	recv(t, calls).answer <- m.env
	next(t, changes)
	// 沒有 Close，背景工作做完也全部結束。
	idle := make(chan struct{})
	go func() { d.wg.Wait(); close(idle) }()
	recv(t, idle)
}

func TestDiscoveryChangesKeepsLatest(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	d.Start()
	recv(t, calls).answer <- m.env
	// 沒人讀的時候表格又變了：讀到的是最新的一份，而且只有一份。
	d.mu.Lock()
	gen := d.gen
	d.mu.Unlock()
	if err := d.waitScanned(context.Background(), gen); err != nil {
		t.Fatal(err)
	}
	other := m.script(filepath.Join(m.root, "opt/foo"), "echo foo 2.0.0")
	d.SetAgents([]Definition{fooDef()}, []Override{{Agent: "foo", Executable: other}})
	if _, err := d.Verify(context.Background(), "foo"); err != nil {
		t.Fatal(err)
	}
	if tab := next(t, changes); tab[0].Path != other {
		t.Fatalf("latest: %+v", tab)
	}
	select {
	case tab := <-changes:
		t.Fatalf("stale table kept: %+v", tab)
	default:
	}
}

func TestDiscoveryVerifyUsesCurrentInputs(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	d.Start()
	recv(t, calls).answer <- m.env
	next(t, changes)
	// 換了覆寫之後、重掃寫入之前，Verify 不得沿用舊世代的結果。
	other := m.script(filepath.Join(m.root, "opt/foo"), "echo foo 2.0.0")
	d.SetAgents([]Definition{fooDef()}, []Override{{Agent: "foo", Executable: other}})
	if in, err := d.Verify(context.Background(), "foo"); err != nil || in.Path != other {
		t.Fatalf("Verify after the override changed: %+v, %v", in, err)
	}
}

func TestDiscoveryCloseInterruptsVerify(t *testing.T) {
	m := newMachine(t)
	started, gate := m.fifo("started"), m.fifo("gate")
	m.script(filepath.Join(m.path, "foo"), "echo > '"+started+"'\nread _ < '"+gate+"'\necho foo 1.0.0")
	d, _ := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	errc := make(chan error, 1)
	go func() {
		_, err := d.Verify(context.Background(), "foo")
		errc <- err
	}()
	recv(t, calls).answer <- m.env
	await(t, started) // 版本探測卡住
	d.Close()
	if err := recv(t, errc); err != ErrClosed {
		t.Fatalf("Verify during Close: %v", err)
	}
}

func TestDiscoveryLaterDetectionWinsWithinGeneration(t *testing.T) {
	m := newMachine(t)
	started, gate := m.fifo("started"), m.fifo("gate")
	flag := filepath.Join(m.root, "verify")
	// 掃描那次讀到 1.0.0 後卡住；之後的執行讀到 2.0.0。
	m.script(filepath.Join(m.path, "foo"), "if [ -e '"+flag+"' ]; then echo foo 2.0.0; exit; fi\necho > '"+started+"'\nread _ < '"+gate+"'\necho foo 1.0.0")
	d, _ := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	d.Start()
	recv(t, calls).answer <- m.env
	await(t, started)
	m.write(flag, "")
	if in, err := d.Verify(context.Background(), "foo"); err != nil || in.Version != "2.0.0" {
		t.Fatalf("Verify: %+v, %v", in, err)
	}
	release(t, gate)
	d.mu.Lock()
	gen := d.gen
	d.mu.Unlock()
	if err := d.waitScanned(context.Background(), gen); err != nil {
		t.Fatal(err)
	}
	// 較早開始的掃描晚寫入，不得蓋過 Verify 的結果。
	if tab := d.Installations(); tab[0].Version != "2.0.0" {
		t.Fatalf("table: %+v", tab)
	}
}

func TestDiscoveryRefreshCancelledStillRescans(t *testing.T) {
	for _, beforeStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "after Start", true: "before Start"}[beforeStart], func(t *testing.T) {
			m := newMachine(t)
			d, changes := m.newDiscovery("", fooDef())
			calls := fakeSnapshots(d)
			if !beforeStart {
				d.Start()
				recv(t, calls).answer <- proc.Env{"HOME=" + m.home, "PATH=/usr/bin:/bin"}
				if tab := next(t, changes); tab[0].Availability != Missing {
					t.Fatalf("first: %+v", tab)
				}
			}
			foo := m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
			// 使用者按了重新整理又離開：等待被取消，快照之後才到。
			ctx, cancel := context.WithCancel(context.Background())
			errc := make(chan error, 1)
			go func() { errc <- d.Refresh(ctx) }()
			snap := recv(t, calls)
			d.mu.Lock()
			ready := d.ready
			d.mu.Unlock()
			if ready {
				t.Fatal("ready before the new snapshot: a scan could use the old environment")
			}
			cancel()
			if err := recv(t, errc); err != context.Canceled {
				t.Fatalf("Refresh: %v", err)
			}
			snap.answer <- m.env
			if tab := next(t, changes); tab[0].Availability != Ready || tab[0].Path != foo {
				t.Fatalf("after the snapshot: %+v", tab)
			}
		})
	}
}

func TestDiscoverySetAgentsUnchangedDoesNotRescan(t *testing.T) {
	m := newMachine(t)
	foo := m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	d.Start()
	recv(t, calls).answer <- m.env
	next(t, changes)
	d.mu.Lock()
	gen := d.gen
	d.mu.Unlock()
	d.SetAgents([]Definition{fooDef()}, nil)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gen != gen || runs(foo) != 1 {
		t.Fatalf("gen %d -> %d, runs %d", gen, d.gen, runs(foo))
	}
}

func TestDiscoveryRefreshDuringScan(t *testing.T) {
	m := newMachine(t)
	started, gate := m.fifo("started"), m.fifo("gate")
	old := m.script(filepath.Join(m.path, "foo"), "echo > '"+started+"'\nread _ < '"+gate+"'\necho foo 1.0.0")
	d, changes := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	d.Start()
	recv(t, calls).answer <- m.env
	await(t, started) // 第一次掃描卡在 foo

	errc := make(chan error, 1)
	go func() { errc <- d.Refresh(context.Background()) }()
	snap := recv(t, calls)
	release(t, gate) // 舊掃描結束：世代已變，不寫入，迴圈停下等新快照
	dir := filepath.Join(m.root, "newpath")
	foo := m.script(filepath.Join(dir, "foo"), "echo foo 2.0.0")
	snap.answer <- proc.Env{"HOME=" + m.home, "PATH=" + dir + ":/usr/bin:/bin"}
	if err := recv(t, errc); err != nil {
		t.Fatal(err)
	}
	if tab := next(t, changes); tab[0].Path != foo || tab[0].Version != "2.0.0" {
		t.Fatalf("after Refresh: %+v", tab)
	}
	if runs(old) != 1 {
		t.Fatalf("old foo ran %d times", runs(old))
	}
}

func TestDiscoverySetAgentsDropsRemovedAgents(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	d.Start()
	recv(t, calls).answer <- m.env
	next(t, changes)
	d.SetAgents(nil, nil)
	if _, err := d.Verify(context.Background(), "foo"); err != ErrUnknownAgent {
		t.Fatalf("Verify of a removed agent: %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.results) != 0 {
		t.Fatalf("results kept: %v", d.results)
	}
}

func TestDiscoveryCloseDropsUnreadTable(t *testing.T) {
	m := newMachine(t)
	m.script(filepath.Join(m.path, "foo"), "echo foo 1.0.0")
	d, changes := m.newDiscovery("", fooDef())
	calls := fakeSnapshots(d)
	d.Start()
	recv(t, calls).answer <- m.env
	d.mu.Lock()
	gen := d.gen
	d.mu.Unlock()
	if err := d.waitScanned(context.Background(), gen); err != nil {
		t.Fatal(err)
	}
	d.Close() // 表格已放進 Changes 但還沒被讀
	if tab, ok := <-changes; ok {
		t.Fatalf("read after Close: %+v", tab)
	}
}
