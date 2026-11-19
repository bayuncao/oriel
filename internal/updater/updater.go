// Package updater 是 OrielHub 的自動更新（ADR-27、D76）：後台定時檢查、
// 菜單欄面板的「檢查更新」與「重啟以更新」。同時最多一次檢查，後台的與
// 用戶按的併成一次；找到新版本就下載安裝（報告下載的百分比），裝好後等用戶重啟。
//
// 它不匯入 mygo：檢查、安裝、重啟與電腦醒來的通知是 app 經 Options 注入的
// 函式（app.NewUpdater，docs/architecture/README.md 規則 8）。規則見
// docs/architecture/hub.md「自動更新」。
package updater

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Phase 是自動更新走到哪一步。
type Phase string

const (
	// Unavailable：這個構建不能自我更新（開發構建、沒配 updates、裝在
	// 不能寫的地方），或還沒 Start；面板不顯示更新按鈕。
	Unavailable Phase = "unavailable"
	// Idle：可以檢查。
	Idle Phase = "idle"
	// Checking：正在問更新源有沒有新版本。
	Checking Phase = "checking"
	// Downloading：找到了新版本，正在下載（收到第一次進度才進入；之前仍是
	// Checking）；Status.Percent 是下載了多少。
	Downloading Phase = "downloading"
	// Installing：下載完了，正在驗簽、解包並替換已安裝的 app。
	Installing Phase = "installing"
	// Ready：新版本已裝好，重啟後使用；之後不再檢查。
	Ready Phase = "ready"
)

// Status 是自動更新目前的狀態，菜單欄面板依它畫更新按鈕。
type Status struct {
	Phase Phase `json:"phase"`
	// Version 是裝好的新版本，只在 Ready 時有。
	Version string `json:"version,omitzero"`
	// Percent 是下載了百分之幾（0–100，向下取整），只在 Downloading 時有；
	// 只在整數百分比變化時更新。差量包沒能用、改下完整包時從 0 重新算。
	Percent int `json:"percent,omitzero"`
	// Full 表示這次檢查換了一份重新下載：差量包沒能用（下載出錯，或下完套不上），
	// 改下完整包；只在 Downloading 時有。頁面據此換文字，讓從 0 重來的百分比
	// 看得出是另一次下載。
	Full bool `json:"full,omitzero"`
	// Rev 每次變化加一：同一份狀態經推送與呼叫結果兩條路到頁面時，取 Rev 大的。
	Rev uint64 `json:"rev"`
}

// Release 是 Options.Check 找到的新版本。
type Release struct {
	Version string
	// Install 下載並驗簽新版本、替換已安裝的 app；正在執行的版本不受影響，
	// 重啟或下次啟動後才是新版本。下載時以已下載與一共的位元組數呼叫
	// progress（可能重複同樣的數）；下完一份（downloaded == total）之後是安裝，
	// 這份不能用而改下另一份時從 0 重新報告。
	Install func(ctx context.Context, progress func(downloaded, total int64)) error
}

// Timer 是排定的計時器；Stop 停掉它，回傳是否停得及。
type Timer interface{ Stop() bool }

// Options 是 Updater 的依賴；mygo 的能力由 app 注入（app.NewUpdater）。
type Options struct {
	// Enabled 回答這個構建能否自我更新，Start 時問一次；nil 視為不能。
	Enabled func() bool
	// Check 問更新源有沒有比正在執行的版本新的版本，已是最新回 nil, nil。
	// Enabled 為真時必填。
	Check func(ctx context.Context) (*Release, error)
	// Relaunch 退出並重新啟動 app，裝好的新版本隨之執行。
	Relaunch func()
	// OnResume 登記電腦醒來時呼叫的函式，回傳取消登記的函式；nil 不理會睡眠。
	OnResume func(fn func()) (off func())
	// Now 是時鐘；nil → time.Now。
	Now func() time.Time
	// Timer 排定計時器；nil → time.AfterFunc。
	Timer func(d time.Duration, f func()) Timer
	// Logger 記錄檢查失敗；nil → 記錄時的 slog.Default()。
	Logger *slog.Logger
}

const (
	// firstWait：啟動或電腦醒來後至少等這麼久才檢查，網路可能還沒好。
	firstWait = 10 * time.Second
	// interval：上次成功的檢查之後多久再查。
	interval = 24 * time.Hour
	// retryWait：檢查失敗後多久重試。
	retryWait = time.Hour
)

// Updater 是自動更新的狀態與排程。方法可從任何 goroutine 呼叫。
type Updater struct {
	o      Options
	ctx    context.Context // 檢查與安裝用；Close 取消
	cancel context.CancelFunc
	wg     sync.WaitGroup // 進行中的檢查

	mu          sync.Mutex
	status      Status
	changed     chan struct{} // 每次變化關閉並換新
	run         *run          // 進行中的檢查；沒有則 nil
	started     bool
	closed      bool
	timer       Timer
	gen         uint64 // 計時器的代：換掉或停掉的計時器晚到時已不是這一代
	offOnResume func()
	// last 是本次啟動最後一次成功檢查的時間，failed 是其後最後一次失敗的
	// 時間（沒有則零值）；checked 表示本次啟動已開始過檢查，之前下一次
	// 總在 firstWait 之後。
	last, failed time.Time
	checked      bool
}

// run 是一次檢查；done 關閉後 status 與 err 是它的結果。
type run struct {
	done   chan struct{}
	status Status
	err    error

	// 下載的進度（u.mu 保護）：reported 之後 downloaded、total 是上一次報告的
	// 位元組數；full 是已換了一份重新下載。
	reported          bool
	downloaded, total int64
	full              bool
}

// New 回傳 Unavailable 的 Updater；Start 之後才可能檢查。
func New(o Options) *Updater {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Timer == nil {
		o.Timer = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Updater{o: o, ctx: ctx, cancel: cancel,
		status: Status{Phase: Unavailable}, changed: make(chan struct{})}
}

// Start 問這個構建能否自我更新：能就變成 Idle，並排定後台檢查
// （firstWait 之後一次，之後見 nextCheck）。只有第一次呼叫有作用；
// Enabled 可能做 I/O，不要在主線程上呼叫。
func (u *Updater) Start() {
	u.mu.Lock()
	if u.started || u.closed {
		u.mu.Unlock()
		return
	}
	u.started = true
	u.mu.Unlock()
	if u.o.Enabled == nil || !u.o.Enabled() {
		return
	}
	var off func()
	if u.o.OnResume != nil {
		// 計時器在電腦睡眠時停走：醒來後按牆鐘重排。
		off = u.o.OnResume(u.resumed)
	}
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		if off != nil {
			off()
		}
		return
	}
	u.offOnResume = off
	u.setLocked(Status{Phase: Idle})
	u.scheduleLocked()
	u.mu.Unlock()
}

// Status 回傳目前的狀態。
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

// Watch 先以目前的狀態呼叫 send，之後每次變化呼叫一次（變化比 send 快時
// 只送最新的），直到 ctx 結束（回 ctx.Err()）、send 出錯（回該錯誤）或
// Close（回 nil）。
func (u *Updater) Watch(ctx context.Context, send func(Status) error) error {
	first := true
	var rev uint64
	for {
		u.mu.Lock()
		s, changed := u.status, u.changed
		u.mu.Unlock()
		if first || s.Rev != rev {
			if err := send(s); err != nil {
				return err
			}
			first, rev = false, s.Rev
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-u.ctx.Done():
			return nil
		}
	}
}

// Check 是用戶按的「檢查更新」：與進行中的檢查（後台的或另一次 Check）
// 併成一次，找到新版本就下載安裝。回傳這次檢查結束時的狀態——Idle 是已是
// 最新，Ready 是已裝好；失敗回 updater.check_failed 或
// updater.install_failed，狀態回到 Idle。不是 Idle 也不在檢查時
// （Unavailable、Ready）不檢查，直接回傳目前的狀態。ctx 結束只結束等待，
// 檢查照常進行。
func (u *Updater) Check(ctx context.Context) (Status, error) {
	u.mu.Lock()
	r := u.beginLocked()
	s := u.status
	u.mu.Unlock()
	if r == nil {
		return s, nil
	}
	select {
	case <-r.done:
		return r.status, r.err
	case <-ctx.Done():
		return Status{}, ctx.Err()
	}
}

// Restart 在新版本裝好（Ready）時重啟 app；其他時候什麼都不做。
func (u *Updater) Restart() {
	u.mu.Lock()
	ready := u.status.Phase == Ready && !u.closed
	u.mu.Unlock()
	if ready {
		u.o.Relaunch()
	}
}

// Close 停掉後台檢查、取消進行中的檢查並等它結束；之後 Watch 回 nil，
// Start 與 Check 不再檢查。可以重複呼叫。
func (u *Updater) Close() {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	u.closed = true
	u.stopTimerLocked()
	off := u.offOnResume
	u.offOnResume = nil
	u.mu.Unlock()
	if off != nil {
		off()
	}
	u.cancel()
	u.wg.Wait()
}

// beginLocked 開始一次檢查；已在檢查時回傳那一次，不是 Idle（或已 Close）
// 時回 nil。
func (u *Updater) beginLocked() *run {
	if u.run != nil {
		return u.run
	}
	if u.closed || u.status.Phase != Idle {
		return nil
	}
	r := &run{done: make(chan struct{})}
	u.run = r
	u.checked = true
	u.stopTimerLocked()
	u.setLocked(Status{Phase: Checking})
	u.wg.Add(1)
	go u.do(r)
	return r
}

// do 跑一次檢查：找到新版本就安裝，結束時寫下結果並排定下一次。
func (u *Updater) do(r *run) {
	defer u.wg.Done()
	rel, err := u.o.Check(u.ctx)
	if err != nil {
		err = fail(CodeCheckFailed, err)
	} else if rel != nil {
		ierr := rel.Install(u.ctx, func(downloaded, total int64) {
			u.reportProgress(r, downloaded, total)
		})
		if ierr != nil {
			err = fail(CodeInstallFailed, ierr)
		}
	}
	now := u.o.Now()
	u.mu.Lock()
	u.run = nil
	switch {
	case u.ctx.Err() != nil:
		// Close：不改狀態，也不再排程。
		err = u.ctx.Err()
	case err != nil:
		u.failed = now
		u.setLocked(Status{Phase: Idle})
	case rel != nil:
		u.setLocked(Status{Phase: Ready, Version: rel.Version})
	default:
		u.last, u.failed = now, time.Time{}
		u.setLocked(Status{Phase: Idle})
	}
	r.status, r.err = u.status, err
	u.scheduleLocked()
	u.mu.Unlock()
	close(r.done)
	if e := (*Error)(nil); errors.As(err, &e) {
		u.logger().Warn("updater: update failed", "code", e.Code, "err", e.cause)
	}
}

// reportProgress 是 Release.Install 報告的進度；第一次報告才從 Checking 進入
// Downloading，所以還沒開始下載就失敗的安裝不顯示下載。大小變了或已下載的變少了，
// 就是換了一份重新下載（mygo 在差量包沒能用時改下完整包），之後的 Downloading
// 都帶 Full。r 已結束或 Close 之後晚到的不理會。
func (u *Updater) reportProgress(r *run, downloaded, total int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.liveLocked(r) {
		return
	}
	if r.reported && (total != r.total || downloaded < r.downloaded) {
		r.full = true
	}
	r.reported, r.downloaded, r.total = true, downloaded, total
	next := downloadStatus(downloaded, total)
	if next.Phase == Downloading {
		next.Full = r.full
	}
	u.progressLocked(next)
}

// liveLocked 回答 r 是否仍是進行中的檢查（沒結束、沒 Close）。
func (u *Updater) liveLocked(r *run) bool { return u.run == r && !u.closed }

// progressLocked 換上下載或安裝的狀態 next；和目前的一樣（同一階段、同一個
// 百分比、同一份下載）時不換，免得每次讀到資料都推送。
func (u *Updater) progressLocked(next Status) {
	cur := u.status
	cur.Rev = 0
	if cur == next {
		return
	}
	u.setLocked(next)
}

// downloadStatus 是下載了 downloaded／total 位元組時的狀態：下完是 Installing。
// total 不大於 0（不知道大小）時停在 0%。
func downloadStatus(downloaded, total int64) Status {
	if total <= 0 {
		return Status{Phase: Downloading}
	}
	if downloaded >= total {
		return Status{Phase: Installing}
	}
	return Status{Phase: Downloading, Percent: int(max(downloaded, 0) * 100 / total)}
}

// scheduleLocked 排定下一次後台檢查；只在 Idle 且沒在檢查時排。
func (u *Updater) scheduleLocked() {
	u.stopTimerLocked()
	if u.closed || u.run != nil || u.status.Phase != Idle {
		return
	}
	wait := firstWait
	if u.checked {
		now := u.o.Now()
		wait = max(nextCheck(u.last, u.failed, now).Sub(now), firstWait)
	}
	gen := u.gen
	u.timer = u.o.Timer(wait, func() { u.fire(gen) })
}

// stopTimerLocked 停掉排定的計時器；它若已在點火，fire 會因代不符而作罷。
func (u *Updater) stopTimerLocked() {
	u.gen++
	if u.timer != nil {
		u.timer.Stop()
		u.timer = nil
	}
}

// fire 是後台檢查的計時器到期。
func (u *Updater) fire(gen uint64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if gen != u.gen {
		return
	}
	u.timer = nil
	u.beginLocked()
}

// resumed 是電腦醒來：計時器在睡眠中停走，按牆鐘重排。
func (u *Updater) resumed() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.scheduleLocked()
}

// setLocked 換上新狀態（Rev 加一）並叫醒 Watch。
func (u *Updater) setLocked(s Status) {
	s.Rev = u.status.Rev + 1
	u.status = s
	close(u.changed)
	u.changed = make(chan struct{})
}

func (u *Updater) logger() *slog.Logger {
	if u.o.Logger != nil {
		return u.o.Logger
	}
	return slog.Default()
}

// nextCheck 是下一次後台檢查的時間：上次成功後一個 interval；其後失敗過、
// 而失敗後一個 retryWait 更晚時取它（剛成功過就不急著重試）。時鐘被調回時
// 以現在為準。
func nextCheck(last, failed, now time.Time) time.Time {
	if last.After(now) {
		last = now
	}
	if failed.After(now) {
		failed = now
	}
	due := last.Add(interval)
	if !failed.IsZero() {
		if retry := failed.Add(retryWait); retry.After(due) {
			due = retry
		}
	}
	return due
}
