package updater

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// 測試都在 synctest 的泡泡裡跑：synctest.Wait 等到其他 goroutine 都停下
// （例如 Check 已在等進行中的那次檢查），不靠 sleep。後台檢查的時間由
// fakeClock 控制；testWait 只防卡死（泡泡裡的時間是假的，卡住時立刻到期）。
const testWait = 30 * time.Second

// fakeClock 是可推進的時鐘：Advance 依到期順序點火計時器；Sleep 讓牆鐘
// 往前走而計時器的剩餘時間不變，像電腦睡著時（單調時鐘停走）。
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c       *fakeClock
	due     time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Timer(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, due: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// Advance 推進時鐘並依到期順序點火；回呼在鎖外、在呼叫者的 goroutine 上執行。
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for {
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.due.After(c.now) && (next == nil || t.due.Before(next.due)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		next.stopped = true
		due = append(due, next.f)
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

// Sleep 是電腦睡了 d。
func (c *fakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		t.due = t.due.Add(d)
	}
}

// pending 是排著的計時器各自還要多久。
func (c *fakeClock) pending() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, t := range c.timers {
		if !t.stopped {
			out = append(out, t.due.Sub(c.now))
		}
	}
	slices.Sort(out)
	return out
}

// checkCall 是一次 Options.Check：測試經 answer 決定它的結果。
type checkCall struct {
	ctx    context.Context
	answer chan answer
}

type answer struct {
	release *Release
	err     error
}

// release 是 Install 不報進度、直接回 installErr 的新版本。
func release(version string, installErr error) *Release {
	return &Release{Version: version, Install: func(context.Context, func(int64, int64)) error { return installErr }}
}

// download 是測試一步步推進的安裝：送進 steps 的每一對 {已下載, 一共} 位元組
// 各報告一次進度；steps 關閉時 Install 回 done 送來的錯誤（沒送則 nil）。
type download struct {
	steps chan [2]int64
	done  chan error
}

func newDownload() *download {
	return &download{steps: make(chan [2]int64), done: make(chan error, 1)}
}

func (d *download) release(version string) *Release {
	return &Release{Version: version, Install: func(ctx context.Context, progress func(int64, int64)) error {
		for {
			select {
			case st, ok := <-d.steps:
				if !ok {
					select {
					case err := <-d.done:
						return err
					default:
						return nil
					}
				}
				progress(st[0], st[1])
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}}
}

type result struct {
	status Status
	err    error
}

// harness 是接上 fakeClock 的 Updater；Check 交給測試回答，Relaunch 只計數。
type harness struct {
	t          *testing.T
	clock      *fakeClock
	u          *Updater
	checks     chan *checkCall
	relaunches atomic.Int32
	resume     func() // Start 登記的醒來回呼
}

func newHarness(t *testing.T, enabled bool) *harness {
	h := &harness{t: t, checks: make(chan *checkCall),
		clock: &fakeClock{now: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)}}
	h.u = New(Options{
		Enabled: func() bool { return enabled },
		Check: func(ctx context.Context) (*Release, error) {
			c := &checkCall{ctx: ctx, answer: make(chan answer)}
			select {
			case h.checks <- c:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case a := <-c.answer:
				return a.release, a.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		Relaunch: func() { h.relaunches.Add(1) },
		OnResume: func(fn func()) func() { h.resume = fn; return func() {} },
		Now:      h.clock.Now,
		Timer:    h.clock.Timer,
		Logger:   slog.New(slog.DiscardHandler),
	})
	t.Cleanup(h.u.Close)
	return h
}

// nextCheck 等下一次 Options.Check。
func (h *harness) nextCheck() *checkCall {
	h.t.Helper()
	select {
	case c := <-h.checks:
		return c
	case <-time.After(testWait):
		h.t.Fatal("no check began")
		return nil
	}
}

// noCheck 確認沒有進行中的檢查（不論它在檢查、下載還是安裝）。
func (h *harness) noCheck() {
	h.t.Helper()
	synctest.Wait()
	h.u.mu.Lock()
	running, s := h.u.run != nil, h.u.status
	h.u.mu.Unlock()
	if running {
		h.t.Fatalf("a check is running: %+v", s)
	}
}

// check 在另一個 goroutine 上呼叫 Check。
func (h *harness) check(ctx context.Context) <-chan result {
	out := make(chan result, 1)
	go func() {
		s, err := h.u.Check(ctx)
		out <- result{s, err}
	}()
	return out
}

func (h *harness) result(res <-chan result) result {
	h.t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(testWait):
		h.t.Fatal("Check did not return")
		return result{}
	}
}

// waitFor 等狀態變成 phase（經 Watch）。
func (h *harness) waitFor(phase Phase) Status {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	found := errors.New("found")
	var last Status
	err := h.u.Watch(ctx, func(s Status) error {
		if last = s; s.Phase == phase {
			return found
		}
		return nil
	})
	if !errors.Is(err, found) {
		h.t.Fatalf("waiting for %s: %v (last %+v)", phase, err, last)
	}
	return last
}

func (h *harness) wantTimers(want ...time.Duration) {
	h.t.Helper()
	if got := h.clock.pending(); !slices.Equal(got, want) {
		h.t.Fatalf("pending timers %v, want %v", got, want)
	}
}

// U01：不能自我更新的構建一直是 Unavailable：不排後台檢查、不理會睡眠，
// Check 不檢查，Restart 不重啟。還沒 Start 時也一樣。
func TestUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		if s, err := h.u.Check(context.Background()); err != nil || s != (Status{Phase: Unavailable}) {
			t.Fatalf("before Start: %+v %v", s, err)
		}
		h.wantTimers()

		h = newHarness(t, false)
		h.u.Start()
		if s := h.u.Status(); s != (Status{Phase: Unavailable}) {
			t.Fatalf("status %+v", s)
		}
		h.wantTimers()
		if h.resume != nil {
			t.Error("watches sleep")
		}
		if s, err := h.u.Check(context.Background()); err != nil || s.Phase != Unavailable {
			t.Fatalf("Check: %+v %v", s, err)
		}
		h.noCheck()
		h.u.Restart()
		if n := h.relaunches.Load(); n != 0 {
			t.Errorf("relaunched %d times", n)
		}
	})
}

// U02–U05：用戶按「檢查更新」的各種結局。檢查時狀態是 Checking；Check 回傳
// 結束時的狀態，與 Status 相同，Rev 比檢查中時大。
func TestCheck(t *testing.T) {
	boom := errors.New("network is down")
	for _, tc := range []struct {
		name   string
		answer answer
		phase  Phase
		ver    string
		code   Code // "" 為成功
	}{
		{name: "U04 已是最新", phase: Idle},
		{name: "U03 找到新版本並裝好", answer: answer{release: release("0.2.0", nil)}, phase: Ready, ver: "0.2.0"},
		{name: "U05 檢查失敗", answer: answer{err: boom}, phase: Idle, code: CodeCheckFailed},
		{name: "U05 下載或安裝失敗", answer: answer{release: release("0.2.0", boom)}, phase: Idle, code: CodeInstallFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, true)
				h.u.Start()
				res := h.check(context.Background())
				c := h.nextCheck()
				checking := h.u.Status()
				if checking.Phase != Checking {
					t.Fatalf("while checking: %+v", checking)
				}
				h.wantTimers()
				c.answer <- tc.answer
				r := h.result(res)
				if r.status.Phase != tc.phase || r.status.Version != tc.ver || r.status.Rev <= checking.Rev {
					t.Fatalf("returned %+v after %+v", r.status, checking)
				}
				if cur := h.u.Status(); cur != r.status {
					t.Fatalf("returned %+v, current %+v", r.status, cur)
				}
				if tc.code == "" {
					if r.err != nil {
						t.Fatalf("error %v", r.err)
					}
					return
				}
				var e *Error
				if !errors.As(r.err, &e) || e.Code != tc.code || e.Params["detail"] != boom.Error() || !errors.Is(r.err, boom) {
					t.Fatalf("error %#v, want %s with the cause", r.err, tc.code)
				}
			})
		})
	}
}

// U02、U06：同時的檢查併成一次——後台檢查進行中，用戶按兩次（例如面板關了
// 又開），都等同一次 Options.Check，得到同一個結果。
func TestChecksJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		h.u.Start()
		h.clock.Advance(firstWait)
		c := h.nextCheck()
		a, b := h.check(context.Background()), h.check(context.Background())
		synctest.Wait()
		select {
		case <-h.checks:
			t.Fatal("a second check began")
		default:
		}
		c.answer <- answer{release: release("0.2.0", nil)}
		for _, r := range []result{h.result(a), h.result(b)} {
			if r.err != nil || r.status.Phase != Ready || r.status.Version != "0.2.0" {
				t.Fatalf("%+v %v", r.status, r.err)
			}
		}
	})
}

// U02：呼叫者離開（關掉面板）只結束它的等待，檢查照常進行；重新打開面板
// 時仍是 Checking。
func TestCheckCallerLeaves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		h.u.Start()
		ctx, cancel := context.WithCancel(context.Background())
		res := h.check(ctx)
		c := h.nextCheck()
		cancel()
		if r := h.result(res); !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Check after the caller left: %+v %v", r.status, r.err)
		}
		if c.ctx.Err() != nil {
			t.Fatal("the check was canceled with its caller")
		}
		if s := h.u.Status(); s.Phase != Checking {
			t.Fatalf("status %+v", s)
		}
		c.answer <- answer{}
		h.waitFor(Idle)
	})
}

// U03：裝好之後 Restart 重啟一次；再按 Check 不再檢查，直接回 Ready；
// 沒裝好時 Restart 什麼都不做。
func TestRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		h.u.Start()
		h.u.Restart()
		if n := h.relaunches.Load(); n != 0 {
			t.Fatalf("relaunched while idle")
		}
		res := h.check(context.Background())
		h.nextCheck().answer <- answer{release: release("0.2.0", nil)}
		ready := h.result(res).status
		if s, err := h.u.Check(context.Background()); err != nil || s != ready {
			t.Fatalf("Check when ready: %+v %v, want %+v", s, err, ready)
		}
		h.noCheck()
		h.u.Restart()
		if n := h.relaunches.Load(); n != 1 {
			t.Fatalf("relaunched %d times", n)
		}
	})
}

// U06 與排程：啟動 10 秒後查；成功後一天、失敗後一小時再查；用戶的檢查成功
// 後從它起算一天；檢查中與裝好後不排；電腦醒來後按牆鐘重排，到期的
// 等 10 秒就查。
func TestSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		h.u.Start()
		h.wantTimers(firstWait)
		h.clock.Advance(firstWait - time.Nanosecond)
		h.noCheck()
		h.clock.Advance(time.Nanosecond)
		c := h.nextCheck()
		h.wantTimers()
		c.answer <- answer{}
		h.waitFor(Idle)
		h.wantTimers(interval)

		h.clock.Advance(interval)
		c = h.nextCheck()
		c.answer <- answer{err: errors.New("offline")}
		h.waitFor(Idle)
		h.wantTimers(retryWait)

		h.clock.Advance(10 * time.Minute)
		res := h.check(context.Background())
		h.nextCheck().answer <- answer{}
		if r := h.result(res); r.err != nil {
			t.Fatal(r.err)
		}
		h.wantTimers(interval)

		h.clock.Sleep(48 * time.Hour)
		h.resume()
		h.wantTimers(firstWait)
		h.clock.Advance(firstWait)
		h.nextCheck().answer <- answer{release: release("0.2.0", nil)}
		h.waitFor(Ready)
		h.wantTimers()
		h.resume()
		h.wantTimers()
	})
}

// Watch 先送目前的狀態，之後每次變化各送一次、Rev 遞增；ctx 結束或 send
// 出錯時回傳。
func TestWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		ctx, cancel := context.WithCancel(context.Background())
		var got []Status
		done := make(chan error, 1)
		go func() {
			done <- h.u.Watch(ctx, func(s Status) error {
				got = append(got, s)
				return nil
			})
		}()
		synctest.Wait()
		h.u.Start()
		synctest.Wait()
		res := h.check(context.Background())
		c := h.nextCheck()
		synctest.Wait()
		d := newDownload()
		c.answer <- answer{release: d.release("0.2.0")}
		synctest.Wait()
		d.steps <- [2]int64{40, 100}
		synctest.Wait()
		d.steps <- [2]int64{100, 100}
		synctest.Wait()
		close(d.steps)
		h.result(res)
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch after cancel: %v", err)
		}
		want := []Status{{Phase: Unavailable}, {Phase: Idle, Rev: 1}, {Phase: Checking, Rev: 2},
			{Phase: Downloading, Percent: 40, Rev: 3}, {Phase: Installing, Rev: 4}, {Phase: Ready, Version: "0.2.0", Rev: 5}}
		if !slices.Equal(got, want) {
			t.Fatalf("sent %+v, want %+v", got, want)
		}
		boom := errors.New("page closed")
		if err := h.u.Watch(context.Background(), func(Status) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("Watch with a failing send: %v", err)
		}
	})
}

// M20–M22、M25：找到新版本後仍是 Checking，第一次報告進度才是 Downloading；
// 百分比向下取整、只在整數百分比
// 變化時換狀態（Rev 不動即沒推送）；下完是 Installing；換一份重新下載（大小
// 變了或已下載的變少了）時回到 Downloading、從頭算並帶 Full；裝好是 Ready，
// 不再帶百分比。
func TestDownloadProgress(t *testing.T) {
	type step struct {
		downloaded, total int64
		phase             Phase
		percent           int
		full, pushed      bool
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{name: "M20、M21、M22 差量包下完套不上，改下完整包", steps: []step{
			{0, 1000, Downloading, 0, false, true},
			{0, 1000, Downloading, 0, false, false},
			{9, 1000, Downloading, 0, false, false},
			{10, 1000, Downloading, 1, false, true},
			{19, 1000, Downloading, 1, false, false},
			{999, 1000, Downloading, 99, false, true},
			{1000, 1000, Installing, 0, false, true},
			{1000, 1000, Installing, 0, false, false},
			{0, 5000, Downloading, 0, true, true},
			{100, 5000, Downloading, 2, true, true},
			{5000, 5000, Installing, 0, false, true},
		}},
		{name: "M22 差量包下到一半出錯，改下完整包", steps: []step{
			{300, 1000, Downloading, 30, false, true},
			{50, 5000, Downloading, 1, true, true},
			{2500, 5000, Downloading, 50, true, true},
		}},
		{name: "M22 改下的一份大小相同，已下載的變少", steps: []step{
			{500, 1000, Downloading, 50, false, true},
			{500, 1000, Downloading, 50, false, false},
			{0, 1000, Downloading, 0, true, true},
			{500, 1000, Downloading, 50, true, true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, true)
				h.u.Start()
				res := h.check(context.Background())
				d := newDownload()
				h.nextCheck().answer <- answer{release: d.release("0.2.0")}
				synctest.Wait()
				prev := h.u.Status()
				if prev.Phase != Checking {
					t.Fatalf("before any progress: %+v", prev)
				}
				for _, st := range tc.steps {
					d.steps <- [2]int64{st.downloaded, st.total}
					synctest.Wait()
					s := h.u.Status()
					if s.Phase != st.phase || s.Percent != st.percent || s.Full != st.full || (s.Rev != prev.Rev) != st.pushed {
						t.Fatalf("after %d/%d: %+v (before %+v), want %s %d%% full=%v pushed=%v",
							st.downloaded, st.total, s, prev, st.phase, st.percent, st.full, st.pushed)
					}
					prev = s
				}
				close(d.steps)
				if r := h.result(res); r.err != nil || r.status != (Status{Phase: Ready, Version: "0.2.0", Rev: r.status.Rev}) {
					t.Fatalf("returned %+v %v", r.status, r.err)
				}
			})
		})
	}
}

// downloadStatus 的規則（純函式）：不知道大小時停在 0%，超出大小算下完。
func TestDownloadStatus(t *testing.T) {
	for _, tc := range []struct {
		downloaded, total int64
		want              Status
	}{
		{0, 0, Status{Phase: Downloading}},
		{50, -1, Status{Phase: Downloading}},
		{-5, 100, Status{Phase: Downloading}},
		{1, 3, Status{Phase: Downloading, Percent: 33}},
		{2, 3, Status{Phase: Downloading, Percent: 66}},
		{3, 3, Status{Phase: Installing}},
		{4, 3, Status{Phase: Installing}},
	} {
		if got := downloadStatus(tc.downloaded, tc.total); got != tc.want {
			t.Errorf("downloadStatus(%d, %d) = %+v, want %+v", tc.downloaded, tc.total, got, tc.want)
		}
	}
}

// M23：下載中途失敗回 updater.install_failed，狀態回到 Idle、不帶百分比，
// 一小時後重試，重試從頭下載，是新的一次檢查，不帶 Full。
func TestDownloadFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		h.u.Start()
		res := h.check(context.Background())
		d := newDownload()
		h.nextCheck().answer <- answer{release: d.release("0.2.0")}
		d.steps <- [2]int64{500, 1000}
		d.done <- errors.New("connection reset")
		close(d.steps)
		r := h.result(res)
		var e *Error
		if !errors.As(r.err, &e) || e.Code != CodeInstallFailed {
			t.Fatalf("error %v", r.err)
		}
		if r.status.Phase != Idle || r.status.Percent != 0 {
			t.Fatalf("status %+v", r.status)
		}
		h.wantTimers(retryWait)
		h.clock.Advance(retryWait)
		d = newDownload()
		h.nextCheck().answer <- answer{release: d.release("0.2.0")}
		d.steps <- [2]int64{10, 1000}
		synctest.Wait()
		if s := h.u.Status(); s.Phase != Downloading || s.Percent != 1 || s.Full {
			t.Fatalf("retry: %+v", s)
		}
	})
}

// M23：安裝在下載任何東西之前就失敗（例如寫不進應用旁邊）時，按鈕不曾顯示
// 下載：Install 執行時仍是 Checking，之後回到 Idle。
func TestInstallFailsBeforeDownload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		h.u.Start()
		res := h.check(context.Background())
		var during Status
		h.nextCheck().answer <- answer{release: &Release{Version: "0.2.0",
			Install: func(context.Context, func(int64, int64)) error {
				during = h.u.Status()
				return errors.New("cannot write next to the app")
			}}}
		r := h.result(res)
		if during.Phase != Checking {
			t.Fatalf("while installing: %+v", during)
		}
		var e *Error
		if !errors.As(r.err, &e) || e.Code != CodeInstallFailed || r.status.Phase != Idle {
			t.Fatalf("returned %+v %v", r.status, r.err)
		}
	})
}

// M26：下載中 Close（退出應用）取消下載；之後晚到的進度不再改狀態。
func TestCloseWhileDownloading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		h.u.Start()
		res := h.check(context.Background())
		late := make(chan struct{})
		h.nextCheck().answer <- answer{release: &Release{Version: "0.2.0",
			Install: func(ctx context.Context, progress func(int64, int64)) error {
				progress(500, 1000)
				<-ctx.Done()
				progress(900, 1000)
				close(late)
				return ctx.Err()
			}}}
		synctest.Wait()
		before := h.u.Status()
		if before.Phase != Downloading || before.Percent != 50 {
			t.Fatalf("while downloading: %+v", before)
		}
		h.u.Close()
		<-late
		if r := h.result(res); !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Check: %+v %v", r.status, r.err)
		}
		if s := h.u.Status(); s != before {
			t.Fatalf("status after Close %+v, want %+v", s, before)
		}
	})
}

// Close 取消進行中的檢查並等它結束：等著的 Check 回 context.Canceled，
// Watch 回 nil，計時器停掉；之後 Start 與 Check 不再檢查。可以重複呼叫。
func TestClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		h.u.Start()
		watched := make(chan error, 1)
		go func() { watched <- h.u.Watch(context.Background(), func(Status) error { return nil }) }()
		res := h.check(context.Background())
		c := h.nextCheck()
		h.u.Close()
		if c.ctx.Err() == nil {
			t.Fatal("the check was not canceled")
		}
		if r := h.result(res); !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Check: %+v %v", r.status, r.err)
		}
		if err := <-watched; err != nil {
			t.Fatalf("Watch: %v", err)
		}
		h.wantTimers()
		h.u.Close()
		h.u.Start()
		if _, err := h.u.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-h.checks:
			t.Fatal("checked after Close")
		default:
		}
		h.wantTimers()
	})

	// Close 在 Start 之前：Start 什麼都不做。
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, true)
		h.u.Close()
		h.u.Start()
		if s := h.u.Status(); s.Phase != Unavailable {
			t.Fatalf("status %+v", s)
		}
		h.wantTimers()
	})
}

// nextCheck 的規則（純函式）。
func TestNextCheck(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		last, failed time.Time
		want         time.Time
	}{
		{"沒查過", time.Time{}, time.Time{}, time.Time{}.Add(interval)},
		{"一小時前成功", now.Add(-time.Hour), time.Time{}, now.Add(23 * time.Hour)},
		{"沒成功過，剛失敗", time.Time{}, now.Add(-time.Minute), now.Add(59 * time.Minute)},
		{"早已到期的檢查剛失敗", now.Add(-2 * interval), now.Add(-time.Minute), now.Add(59 * time.Minute)},
		{"剛成功過，用戶的檢查失敗", now.Add(-time.Hour), now.Add(-time.Minute), now.Add(23 * time.Hour)},
		{"時鐘被調回", now.Add(time.Hour), time.Time{}, now.Add(interval)},
		{"時鐘被調回，失敗在未來", time.Time{}, now.Add(time.Hour), now.Add(retryWait)},
	} {
		if got := nextCheck(tc.last, tc.failed, now); !got.Equal(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
