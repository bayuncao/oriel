package agent

import (
	"context"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// Discovery 保管目錄中各 Agent 的 Installation 表格，以及
// 它們被探測與啟動時所用的環境快照。時序規則見
// docs/architecture/agent.md（「發現」）。
//
// 每次輸入變更（環境改變的快照、新 agent 或覆寫、Refresh）都開啟
// 一個新世代。結果只在算出它的世代仍是目前世代時寫入，同一世代內
// 較晚開始的偵測優先，因此舊輸入或較早的結果絕不會蓋過較新的。
// 完整掃描只有一個迴圈在跑：掃描期間輸入又變了，它就以目前的輸入
// 再掃一次，多次變更合併成一次重掃。
type Discovery struct {
	det       *detector
	takeEnv   func(context.Context, proc.Env) (proc.Env, error) // 測試的接縫；預設是 Snapshotter.Snapshot
	root      string
	inherited proc.Env
	budget    time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	closed    bool
	started   bool
	ready     bool // 快照已到，或第一次掃描的等待時間已過
	scanning  bool // 完整掃描迴圈正在跑
	gen       int
	seq       int                 // 每次取輸入時遞增，排定同一世代內結果的先後
	scanned   int                 // 最後一次寫入的完整掃描所屬的世代
	scanDone  chan struct{}       // 每次完整掃描寫入時關閉並換新；Close 時關閉
	changes   chan []Installation // 容量 1：只放最新的表格；Close 時關閉
	sent      []Installation      // 最後放進 changes 的表格
	published bool
	defs      []Definition
	overrides map[task.AgentID]Override
	env       proc.Env
	envDone   chan struct{}      // 目前快照完成時關閉
	envCancel context.CancelFunc // 取消目前的快照
	results   map[task.AgentID]result
}

// result 是一個 Agent 的偵測結果，連同算出它的輸入。
type result struct {
	detection
	gen int
	seq int
}

// DiscoveryOptions 設定一個 Discovery；由 app 提供。
type DiscoveryOptions struct {
	Runner    *proc.Runner
	Root      string   // 前綴加到系統目錄上；"" 表示 /
	Inherited proc.Env // OrielHub 自己的環境
	// Previous 是上一次的表格：第一次寫入前 Installations 回傳它。
	Previous []Installation
}

// NewDiscovery 回傳尚未開始的 Discovery。它沒有常駐的 goroutine：
// 背景工作做完就結束；Close 會中止仍在進行的探測。
func NewDiscovery(o DiscoveryOptions) *Discovery {
	ctx, cancel := context.WithCancel(context.Background())
	snap := &proc.Snapshotter{Runner: o.Runner, Root: o.Root}
	d := &Discovery{
		det:       &detector{runner: o.Runner, root: o.Root},
		takeEnv:   snap.Snapshot,
		root:      o.Root,
		inherited: o.Inherited,
		budget:    2 * time.Second,
		ctx:       ctx,
		cancel:    cancel,
		scanned:   -1,
		scanDone:  make(chan struct{}),
		changes:   make(chan []Installation, 1),
		overrides: map[task.AgentID]Override{},
		env:       fallbackEnv(o.Inherited, o.Root),
		results:   map[task.AgentID]result{},
	}
	for _, in := range o.Previous {
		d.results[in.Agent] = result{detection: detection{inst: in}, gen: -1}
	}
	return d
}

// fallbackEnv 是繼承的環境加上附加到 PATH 的使用者工具目錄，
// 在快照送達之前、或沒有 shell 回應時使用。
func fallbackEnv(inherited proc.Env, root string) proc.Env {
	return inherited.With("PATH=" + strings.Join(proc.SearchDirs(inherited, root), string(filepath.ListSeparator)))
}

// SetAgents 取代目錄與使用者的覆寫；有變化且 discovery 已開始時
// 重新掃描（快照或第一次掃描的等待時間到了才開始）。移出目錄的
// Agent 的結果一併刪除。
func (d *Discovery) SetAgents(defs []Definition, overrides []Override) {
	byAgent := map[task.AgentID]Override{}
	for _, o := range overrides {
		byAgent[o.Agent] = o
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if reflect.DeepEqual(defs, d.defs) && maps.Equal(byAgent, d.overrides) {
		return
	}
	d.defs, d.overrides = slices.Clone(defs), byAgent
	maps.DeleteFunc(d.results, func(id task.AgentID, _ result) bool {
		return !slices.ContainsFunc(d.defs, func(def Definition) bool { return def.ID == id })
	})
	if d.published && !d.closed { // 移出目錄的 Agent 立刻從已送出的表格消失
		d.offerLocked()
	}
	d.gen++
	d.kickLocked()
}

// Start 向各 shell 索取快照並在背景掃描。第一次掃描最多等待
// 快照 budget 的時間，之後改用 fallback 環境；遲到且改變了環境的
// 快照會開啟新世代並觸發重新掃描。Refresh 之後再 Start 不做事。
func (d *Discovery) Start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || d.closed {
		return
	}
	d.started = true
	first := d.snapshotLocked()
	d.goLocked(func(ctx context.Context) {
		wait, cancel := context.WithTimeout(ctx, d.budget)
		defer cancel()
		_, _ = d.Env(wait)
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.envDone == first { // 期間被 Refresh 取代時，由新快照決定何時就緒
			d.ready = true
			d.kickLocked()
		}
	})
}

// Refresh 忘掉失敗的探測、擷取新快照，並在依新快照的完整掃描寫入
// 後回傳。新快照到達前不掃描；ctx 結束只停止等待，重掃照常進行。
// 它也會開始 discovery（不等 budget，直接等快照）。
func (d *Discovery) Refresh(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return ErrClosed
	}
	d.det.forget()
	d.gen++ // 失敗紀錄已清除：即使環境沒變也要重新探測
	d.started = true
	d.ready = false
	d.resnapshotLocked()
	d.mu.Unlock()
	if _, err := d.Env(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	target := d.gen
	d.mu.Unlock()
	return d.waitScanned(ctx, target)
}

// CurrentEnv 回傳目前已就位的環境快照（fallback 或真快照），
// 不阻塞：尚未就緒時回傳 nil。給 registry 的 proxy 函式用。
func (d *Discovery) CurrentEnv() proc.Env {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.env
}

// Env 等待啟動 agent 所用的環境快照；在 Start 之前呼叫時先開始
// 取快照。等待中的快照被 Refresh 取代時，改等取代它的那一次。
func (d *Discovery) Env(ctx context.Context) (proc.Env, error) {
	for {
		d.mu.Lock()
		done := d.snapshotLocked()
		d.mu.Unlock()
		if done == nil {
			return nil, ErrClosed
		}
		select {
		case <-done:
		case <-ctx.Done():
			return nil, d.closedOr(ctx.Err())
		}
		d.mu.Lock()
		closed, current, env := d.closed, d.envDone == done, d.env
		d.mu.Unlock()
		switch {
		case closed:
			return nil, ErrClosed
		case current:
			return env, nil
		}
	}
}

// Installations 是依目錄順序排列的表格。
func (d *Discovery) Installations() []Installation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.installationsLocked()
}

func (d *Discovery) installationsLocked() []Installation {
	out := make([]Installation, 0, len(d.defs))
	for _, def := range d.defs {
		if r, ok := d.results[def.ID]; ok {
			out = append(out, r.inst)
		}
	}
	return out
}

// Verify 在 Session 開始前重新檢查一個 Agent：目前世代的結果中，
// 選定的可執行檔仍存在且未改變時沿用已知版本；其他情況以目前的
// 輸入重新偵測，不等待其他掃描。Close 會中止它。
func (d *Discovery) Verify(ctx context.Context, id task.AgentID) (Installation, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(d.ctx, cancel)()
	for {
		if _, err := d.Env(ctx); err != nil {
			return Installation{}, err
		}
		d.mu.Lock()
		closed := d.closed
		in := d.agentInputsLocked([]task.AgentID{id})
		known, ok := d.results[id]
		d.mu.Unlock()
		switch {
		case closed:
			return Installation{}, ErrClosed
		case len(in.defs) == 0:
			return Installation{}, ErrUnknownAgent
		case ok && known.gen == in.gen && known.inst.Path != "" && known.fp.exec == known.inst.Path && known.fp.current():
			return known.inst, nil
		}
		dets := d.detectEach(ctx, in)
		if err := ctx.Err(); err != nil {
			return Installation{}, d.closedOr(err)
		}
		if inst, ok := d.commitVerified(in, dets, id); ok {
			d.retryLater(timedOut(dets))
			return inst, nil
		}
		// 偵測期間輸入變了或已 Close：回到開頭以目前的狀態重做。
	}
}

// Close 停止背景工作並等待其結束；仍在執行的探測透過其 context
// 被終止。之後 Changes 被關閉，Refresh、Env 與 Verify 回傳
// ErrClosed，包括 Close 時正在等待的呼叫。可從任何 goroutine 呼叫，
// 包括讀 Changes 的那一個。
func (d *Discovery) Close() {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.scanDone)
		select { // 沒讀走的表格丟棄：Close 之後讀不到任何表格
		case <-d.changes:
		default:
		}
		close(d.changes)
	}
	d.mu.Unlock()
	d.cancel()
	d.wg.Wait()
}

// closedOr 在已 Close 時回傳 ErrClosed，否則回傳 err。
func (d *Discovery) closedOr(err error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrClosed
	}
	return err
}

// snapshotLocked 回傳在目前快照（或無人回應時的 fallback）就位時
// 關閉的 channel，還沒有快照時先開始一次；Close 之後回傳 nil。
func (d *Discovery) snapshotLocked() chan struct{} {
	switch {
	case d.closed:
		return nil
	case d.envDone != nil:
		return d.envDone
	}
	return d.resnapshotLocked()
}

// resnapshotLocked 取消進行中的快照並開始新的一次。只有仍是目前
// 那一次的快照會寫入環境：環境真的改變時開啟新世代，並標記就緒、
// 觸發掃描。Close 之後回傳 nil。
func (d *Discovery) resnapshotLocked() chan struct{} {
	if d.closed {
		return nil
	}
	if d.envCancel != nil {
		d.envCancel()
	}
	ctx, cancel := context.WithCancel(d.ctx)
	done := make(chan struct{})
	d.envDone, d.envCancel = done, cancel
	d.wg.Go(func() {
		defer close(done)
		defer cancel()
		env, err := d.takeEnv(ctx, d.inherited)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			env = fallbackEnv(d.inherited, d.root)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.envDone != done || d.closed {
			return
		}
		if !slices.Equal(env, d.env) {
			d.env = env
			d.gen++
		}
		d.ready = true
		d.kickLocked()
	})
	return done
}

// kickLocked 在需要時啟動完整掃描迴圈：discovery 已開始且就緒、
// 目前世代還沒掃過、迴圈沒有在跑。
func (d *Discovery) kickLocked() {
	if d.closed || !d.started || !d.ready || d.scanning || d.scanned == d.gen {
		return
	}
	d.scanning = true
	d.goLocked(d.scanLoop)
}

// scanLoop 以目前的輸入做完整掃描，直到最後一次寫入的掃描所屬的
// 世代就是目前世代；Refresh 等待新快照期間（不就緒）停下，由快照
// 到達時重新觸發。
func (d *Discovery) scanLoop(ctx context.Context) {
	for {
		d.mu.Lock()
		if d.closed || !d.ready || d.scanned == d.gen {
			d.scanning = false
			d.mu.Unlock()
			return
		}
		in := d.allInputsLocked()
		d.mu.Unlock()
		dets := d.detectEach(ctx, in)
		if d.commitScan(in, dets) {
			d.retryLater(timedOut(dets))
		}
	}
}

// waitScanned 等待世代 target 或更新的完整掃描寫入。
func (d *Discovery) waitScanned(ctx context.Context, target int) error {
	for {
		d.mu.Lock()
		closed, done, ch := d.closed, d.scanned >= target, d.scanDone
		d.mu.Unlock()
		switch {
		case closed:
			return ErrClosed
		case done:
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return d.closedOr(ctx.Err())
		}
	}
}

// inputs 是一次偵測所用的輸入。
type inputs struct {
	gen, seq  int
	env       proc.Env
	defs      []Definition
	overrides map[task.AgentID]Override
}

// allInputsLocked 回傳目錄中每個 Agent 的目前輸入；agentInputsLocked
// 只含 ids 中仍在目錄裡的那些。
func (d *Discovery) allInputsLocked() inputs {
	return d.newInputsLocked(slices.Clone(d.defs))
}

func (d *Discovery) agentInputsLocked(ids []task.AgentID) inputs {
	return d.newInputsLocked(slices.DeleteFunc(slices.Clone(d.defs), func(def Definition) bool { return !slices.Contains(ids, def.ID) }))
}

func (d *Discovery) newInputsLocked(defs []Definition) inputs {
	d.seq++
	return inputs{gen: d.gen, seq: d.seq, env: d.env, defs: defs, overrides: d.overrides}
}

// probeConcurrency 是同時偵測的 Agent 數上限。
const probeConcurrency = 4

func (d *Discovery) detectEach(ctx context.Context, in inputs) []detection {
	results := make([]detection, len(in.defs))
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for i, def := range in.defs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = d.det.detect(ctx, in.env, def, in.overrides[def.ID])
		})
	}
	wg.Wait()
	return results
}

// commitScan 寫入一次完整掃描並標記其世代已掃過；commitAgents 與
// commitVerified 只寫入部分 Agent。它們在世代已變或已 Close 時都不
// 寫，回傳 false。
func (d *Discovery) commitScan(in inputs, dets []detection) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.commitLocked(in, dets) {
		return false
	}
	d.scanned = in.gen
	close(d.scanDone)
	d.scanDone = make(chan struct{})
	return true
}

func (d *Discovery) commitAgents(in inputs, dets []detection) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.commitLocked(in, dets)
}

// commitVerified 寫入 Verify 的結果，並在同一段鎖內回傳表中 id 的
// 結果：同一世代內較晚開始的偵測已寫入時是那一個。
func (d *Discovery) commitVerified(in inputs, dets []detection, id task.AgentID) (Installation, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.commitLocked(in, dets) {
		return Installation{}, false
	}
	return d.results[id].inst, true
}

// commitLocked 在 in 仍是目前世代時寫入結果（同一 Agent 已有較晚
// 開始的結果時略過）並通知發佈。
func (d *Discovery) commitLocked(in inputs, dets []detection) bool {
	if d.closed || in.gen != d.gen {
		return false
	}
	for _, det := range dets {
		if old, ok := d.results[det.inst.Agent]; ok && old.gen == in.gen && old.seq > in.seq {
			continue
		}
		d.results[det.inst.Agent] = result{detection: det, gen: in.gen, seq: in.seq}
	}
	d.offerLocked()
	return true
}

func timedOut(dets []detection) []task.AgentID {
	var out []task.AgentID
	for _, det := range dets {
		if det.timedOut {
			out = append(out, det.inst.Agent)
		}
	}
	return out
}

// retryLater 在背景以當時的輸入重試一次探測逾時的 Agent。
func (d *Discovery) retryLater(ids []task.AgentID) {
	if len(ids) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.goLocked(func(ctx context.Context) {
		d.mu.Lock()
		in := d.agentInputsLocked(ids)
		d.mu.Unlock()
		if len(in.defs) > 0 {
			d.commitAgents(in, d.detectEach(ctx, in))
		}
	})
}

// Changes 送出表格：第一次寫入時，以及之後內容（不計探測時間）
// 每次變更時。它只保留最新的一份，讀得慢的一方直接拿到最新的
// 表格，不會漏掉最後一次變更；Close 時丟棄未讀的表格並關閉。應只有
// 一個讀取者。收到的表格與內部共用，只能讀、不可修改。
func (d *Discovery) Changes() <-chan []Installation { return d.changes }

// offerLocked 把目前的表格放進 changes，取代尚未被讀走的舊表格。
// 只有持有 d.mu 的一方會送，因此先取出再放入永遠不會阻塞。
func (d *Discovery) offerLocked() {
	tab := d.installationsLocked()
	if d.published && sameTable(d.sent, tab) {
		return
	}
	d.sent, d.published = tab, true
	select {
	case <-d.changes:
	default:
	}
	d.changes <- tab
}

// goLocked 在持有 d.mu 的情況下啟動 f，除非 Discovery 已關閉：
// Close 在等待之前以同一把鎖設定 closed，因此不會有 goroutine
// 被加到正在被等待的 WaitGroup 上。
func (d *Discovery) goLocked(f func(ctx context.Context)) {
	if !d.closed {
		d.wg.Go(func() { f(d.ctx) })
	}
}

// sameTable 比較兩份表格，忽略它們的探測時間。
func sameTable(a, b []Installation) bool {
	strip := func(in []Installation) []Installation {
		out := make([]Installation, len(in))
		for i, x := range in {
			x.ProbedAt = time.Time{}
			out[i] = x
		}
		return out
	}
	return reflect.DeepEqual(strip(a), strip(b))
}
