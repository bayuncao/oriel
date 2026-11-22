package usage

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

const (
	// interval：上一輪讀完之後多久再讀一輪（D86）。
	interval = 24 * time.Hour
	// retryWait：一輪沒讀成（拿不到環境快照或讀取位置）之後多久再試。
	retryWait = time.Hour
	// headBytes：Lines 格式的檔案用開頭這麼多位元組認出是不是同一個檔案。
	headBytes = 4096
	// 一段至多讀這麼多位元組或用量，讀到的用量與讀取位置在一個事務裡寫入。
	defaultChunkBytes   = 8 << 20
	defaultChunkRecords = 2000
	// reportWait 是寫一份報來的額度的時限：Close 等寫入中的那份寫完，資料庫卡住時不能讓退出跟著卡住。
	reportWait = 5 * time.Second
)

// Timer 是排定的計時器；Stop 停掉它，回傳是否停得及。
type Timer interface{ Stop() bool }

// Options 是 Tracker 的依賴；mygo 的能力（電腦醒來的通知）由 app 注入。
type Options struct {
	// Sources 依記錄來源列出的順序。
	Sources []Source
	Store   Store
	// Env 等環境快照（agent.Discovery.Env）；nil → proc.Inherited()。
	Env func(ctx context.Context) (proc.Env, error)
	// Now 是時鐘；nil → time.Now。
	Now func() time.Time
	// Timer 排定計時器；nil → time.AfterFunc。
	Timer func(d time.Duration, f func()) Timer
	// OnResume 登記電腦醒來時呼叫的函式，回傳取消登記的函式；nil 不理會睡眠。
	OnResume func(fn func()) (off func())
	// Zone 是現在的本機時區，每次彙總都重新取（U09）；nil → SystemZone。
	Zone func() *time.Location
	// Logger 記錄讀取失敗（不含記錄內容）；nil → 記錄時的 slog.Default()。
	Logger *slog.Logger
	// ChunkBytes、ChunkRecords 是一段的上限；0 → 8 MiB、2,000 條。
	ChunkBytes   int64
	ChunkRecords int
}

// ReadState 是讀取的進度，頁面據此顯示進行中與上次讀完的時間。
type ReadState struct {
	Reading bool `json:"reading"`
	// Done、Total 是這一輪已讀與要讀的位元組數（磁碟上的大小）。
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
	// ReadAt 是上一輪讀完的時間；還沒讀完過一輪時為零值。
	ReadAt time.Time `json:"readAt,omitzero"`
	// QuotaAt 是最近一次寫入的、agent 經協議報來的額度快照的時間（Report，m3.md CQ10）：
	// 變了頁面就重取彙總；還沒有時為零值。
	QuotaAt time.Time `json:"quotaAt,omitzero"`
	// Rev 每次變化加一：同一份狀態經推送與呼叫結果兩條路到頁面時，取 Rev 大的。
	Rev uint64 `json:"rev"`
}

// Tracker 讀各 Source 的記錄、排程並彙總。方法可從任何 goroutine 呼叫。
type Tracker struct {
	o      Options
	ctx    context.Context // 讀取用；Close 取消
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	state   ReadState
	changed chan struct{} // 每次變化關閉並換新
	// want 是要求過的輪數，done 是讀完的輪數；一輪開始時把當時的 want 當作目標。
	want, done uint64
	running    bool
	started    bool
	closed     bool
	timer      Timer
	gen        uint64 // 計時器的代
	offResume  func()
	infos      map[task.AgentID]SourceInfo // 上一輪的記錄來源
	retry      bool                        // 上一輪沒讀成
	// reported 是 Report 收下、還沒寫入的額度快照，每個桶只留最新的一份；writing 是
	// writeReported 在跑；quotaAt 是寫入過的最晚的快照時間（ReadState.QuotaAt）。
	reported map[quotaKey]Quota
	writing  bool
	quotaAt  time.Time
}

// New 回傳還沒開始讀的 Tracker；Start 之後才讀。
func New(o Options) *Tracker {
	if o.Env == nil {
		o.Env = func(context.Context) (proc.Env, error) { return proc.Inherited(), nil }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Timer == nil {
		o.Timer = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	if o.Zone == nil {
		o.Zone = SystemZone
	}
	o.ChunkBytes = cmp.Or(o.ChunkBytes, defaultChunkBytes)
	o.ChunkRecords = cmp.Or(o.ChunkRecords, defaultChunkRecords)
	ctx, cancel := context.WithCancel(context.Background())
	return &Tracker{o: o, ctx: ctx, cancel: cancel, changed: make(chan struct{})}
}

// Start 讀一輪，之後每次讀完 24 小時再讀一輪。只有第一次呼叫有作用。
func (t *Tracker) Start() {
	t.mu.Lock()
	if t.started || t.closed {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.mu.Unlock()
	var off func()
	if t.o.OnResume != nil {
		// 計時器在電腦睡眠時停走：醒來後按牆鐘重排（U16）。
		off = t.o.OnResume(t.resumed)
	}
	t.mu.Lock()
	if t.closed {
		// 登記的途中被 Close 了：Close 沒看到 off，這裡取消登記。不持鎖：off 可能等正在跑的 resumed。
		t.mu.Unlock()
		if off != nil {
			off()
		}
		return
	}
	t.offResume = off
	t.requestLocked()
	t.mu.Unlock()
}

// Refresh 讀一輪：與進行中的讀取合併，讀到呼叫之前追加的內容後才返回（U12）。
// ctx 結束只結束等待，讀取照常進行。
func (t *Tracker) Refresh(ctx context.Context) (ReadState, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ReadState{}, errClosed
	}
	target := t.requestLocked()
	t.mu.Unlock()
	for {
		t.mu.Lock()
		s, done, closed, changed := t.state, t.done, t.closed, t.changed
		t.mu.Unlock()
		if done >= target {
			return s, nil
		}
		if closed {
			return ReadState{}, errClosed
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ReadState{}, ctx.Err()
		}
	}
}

// Watch 先以目前的狀態呼叫 send，之後每次變化呼叫一次（變化比 send 快時只送
// 最新的），直到 ctx 結束（回 ctx.Err()）、send 出錯（回該錯誤）或 Close（回 nil）。
func (t *Tracker) Watch(ctx context.Context, send func(ReadState) error) error {
	first := true
	var rev uint64
	for {
		t.mu.Lock()
		s, changed, closed := t.state, t.changed, t.closed
		t.mu.Unlock()
		if first || s.Rev != rev {
			if err := send(s); err != nil {
				return err
			}
			first, rev = false, s.Rev
		}
		if closed {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Close 停掉排程、取消進行中的讀取並等它結束；之後 Refresh 回 usage.closed，
// Watch 回 nil。可以重複呼叫。
func (t *Tracker) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.stopTimerLocked()
	off := t.offResume
	t.offResume = nil
	t.notifyLocked()
	t.mu.Unlock()
	if off != nil {
		off()
	}
	t.cancel()
	t.wg.Wait()
}

// requestLocked 要求一輪並確保讀取迴圈在跑，回傳這次要求的輪號。也叫醒等待者：
// 狀態沒變（Rev 不變），Watch 不會因此多送。
func (t *Tracker) requestLocked() uint64 {
	t.want++
	t.notifyLocked()
	if !t.running && !t.closed {
		t.running = true
		t.stopTimerLocked()
		t.wg.Add(1)
		go t.loop()
	}
	return t.want
}

// loop 一輪接一輪地讀，直到沒有新的要求；讀的過程中來的要求合成下一輪。
func (t *Tracker) loop() {
	defer t.wg.Done()
	for {
		t.mu.Lock()
		if t.closed || t.done >= t.want {
			t.running = false
			t.scheduleLocked()
			t.mu.Unlock()
			return
		}
		target := t.want
		t.setLocked(ReadState{Reading: true, ReadAt: t.state.ReadAt})
		t.mu.Unlock()

		infos, ok := t.round(t.ctx)

		t.mu.Lock()
		if t.ctx.Err() != nil {
			t.running = false
			t.mu.Unlock()
			return
		}
		t.done = target
		if ok {
			t.infos, t.retry = infos, false
			t.setLocked(ReadState{ReadAt: t.o.Now()})
		} else {
			// 拿不到環境快照或讀取位置：上一輪讀完的時間不變，一小時後再試。
			t.retry = true
			t.setLocked(ReadState{ReadAt: t.state.ReadAt})
		}
		t.mu.Unlock()
	}
}

// scheduleLocked 排定下一輪：上一輪讀完 24 小時後；上一輪沒讀成時一小時後。
func (t *Tracker) scheduleLocked() {
	t.stopTimerLocked()
	if t.closed || t.running || (!t.retry && t.state.ReadAt.IsZero()) {
		return
	}
	wait := retryWait
	if !t.retry {
		wait = max(t.state.ReadAt.Add(interval).Sub(t.o.Now()), 0)
	}
	gen := t.gen
	t.timer = t.o.Timer(wait, func() { t.fire(gen) })
}

func (t *Tracker) stopTimerLocked() {
	t.gen++
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

func (t *Tracker) fire(gen uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.gen || t.closed {
		return
	}
	t.timer = nil
	t.requestLocked()
}

// resumed 是電腦醒來：計時器在睡眠中停走，按牆鐘重排。
func (t *Tracker) resumed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.scheduleLocked()
}

// setLocked 換上新狀態（Rev 加一）並叫醒等待者。
func (t *Tracker) setLocked(s ReadState) {
	s.QuotaAt = t.quotaAt
	s.Rev = t.state.Rev + 1
	t.state = s
	t.notifyLocked()
}

func (t *Tracker) notifyLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func (t *Tracker) progress(done, total int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.state
	s.Done, s.Total = done, total
	t.setLocked(s)
}

// Report 收下 agent 經協議報來的額度快照（Claude 的 rate_limit_event，D87、m3.md CQ），標上
// Reported：不阻塞，在背景寫入同一張額度表；同一個桶只寫最新的一份（CQ08）。寫好後
// ReadState.QuotaAt 換成它的時間，頁面據此重取彙總（CQ10）。寫入失敗時記日誌、留著這份，
// 下一次 Report 時再寫（CQ09）。不必先 Start；Close 之後的丟掉，Close 等寫入中的那份
// 寫完（至多 reportWait）。
func (t *Tracker) Report(q Quota) {
	q.Windows = slices.Clone(q.Windows)
	q.Reported = true
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	k := quotaKey{q.Agent, q.Bucket}
	if old, ok := t.reported[k]; ok && old.At.After(q.At) {
		return
	}
	if t.reported == nil {
		t.reported = map[quotaKey]Quota{}
	}
	t.reported[k] = q
	if !t.writing {
		t.writing = true
		t.wg.Add(1)
		go t.writeReported()
	}
}

// writeReported 寫出 Report 收下的快照，直到沒有新的。Close 取消讀取用的 ctx，但寫入中的
// 這份照樣寫完（一行，很快；至多 reportWait），免得退出前最後報來的額度丟掉。寫不進去時把
// 這幾份放回去（同一個桶已有更晚的就不放）並停下：不原地重試，等下一次 Report 再寫。
func (t *Tracker) writeReported() {
	defer t.wg.Done()
	for {
		t.mu.Lock()
		if len(t.reported) == 0 {
			t.writing = false
			t.mu.Unlock()
			return
		}
		quotas := slices.Collect(maps.Values(t.reported))
		clear(t.reported)
		t.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.ctx), reportWait)
		err := t.o.Store.ApplyUsage(ctx, Batch{Quotas: quotas})
		cancel()
		t.mu.Lock()
		if err != nil {
			t.logger().Warn("usage: saving a reported quota failed", "err", err)
			for _, q := range quotas {
				k := quotaKey{q.Agent, q.Bucket}
				if cur, ok := t.reported[k]; !ok || q.At.After(cur.At) {
					t.reported[k] = q
				}
			}
			t.writing = false
			t.mu.Unlock()
			return
		}
		latest := t.quotaAt
		for _, q := range quotas {
			if q.At.After(latest) {
				latest = q.At
			}
		}
		if latest.After(t.quotaAt) {
			t.quotaAt = latest
			t.setLocked(t.state)
		}
		t.mu.Unlock()
	}
}

func (t *Tracker) logger() *slog.Logger {
	return cmp.Or(t.o.Logger, slog.Default())
}

// work 是這一輪要讀的一個檔案。
type work struct {
	src    Source
	file   File
	stat   fileStat
	stored *FileState
}

type fileStat struct {
	size    int64
	modTime time.Time
}

// round 讀一輪：每個 Source 找出記錄檔，讀變了的部分，刪掉已不在的檔案的讀取
// 位置。回傳各 agent 的記錄來源；拿不到環境快照或讀取位置時 ok 為假。
func (t *Tracker) round(ctx context.Context) (infos map[task.AgentID]SourceInfo, ok bool) {
	env, err := t.o.Env(ctx)
	if err != nil {
		return nil, false
	}
	storedList, err := t.o.Store.UsageFiles(ctx)
	if err != nil {
		t.logger().Warn("usage: reading positions failed", "err", err)
		return nil, false
	}
	stored := map[string]*FileState{}
	for i := range storedList {
		stored[storedList[i].Path] = &storedList[i]
	}

	infos = map[task.AgentID]SourceInfo{}
	owner := map[string]task.AgentID{} // 這一輪仍在的記錄檔與它的 agent
	bad := map[string]int{}            // 記錄檔裡沒讀懂的行數
	var todo []work
	var total int64
	for _, src := range t.o.Sources {
		agent := src.Agent()
		info := SourceInfo{Agent: agent, Unreported: src.Unreported(), Notes: src.Notes(), Searched: true}
		roots, files, err := src.Find(env)
		info.Roots, info.Files = roots, len(files)
		if err != nil {
			path := ""
			if pe := (*os.PathError)(nil); errors.As(err, &pe) {
				path = pe.Path
			} else if len(roots) > 0 {
				path = roots[0]
			}
			info.Problem = problemOf(readFailed(string(agent), path, err))
			t.logger().Warn("usage: finding records failed", "agent", agent, "code", CodeReadFailed, "err", err)
			// 找不全時不刪這個 agent 的讀取位置：檔案可能還在，只是這次讀不到。
			for _, s := range storedList {
				if s.Agent == agent {
					owner[s.Path] = agent
					bad[s.Path] = s.Bad
				}
			}
		}
		infos[agent] = info
		for _, f := range files {
			owner[f.Path] = agent
			s := stored[f.Path]
			if s != nil && s.Agent != agent {
				s = nil
			}
			if s != nil {
				bad[f.Path] = s.Bad
			}
			st, err := statFile(f)
			if err != nil {
				continue // 剛被刪掉或換掉；下一輪再看
			}
			if s != nil && s.Done && s.Size == st.size && s.ModTime.Equal(st.modTime) {
				continue
			}
			w := work{src: src, file: f, stat: st, stored: s}
			total += w.cost()
			todo = append(todo, w)
		}
	}
	var gone []string
	for _, s := range storedList {
		if _, ok := owner[s.Path]; !ok {
			gone = append(gone, s.Path)
		}
	}
	if len(gone) > 0 {
		if err := t.o.Store.DeleteUsageFiles(ctx, gone); err != nil {
			t.logger().Warn("usage: deleting positions failed", "err", err)
		}
	}

	t.progress(0, total)
	var done int64
	dirs := map[string]string{} // 這一輪解析過的目錄
	for _, w := range todo {
		if ctx.Err() != nil {
			return nil, false
		}
		r := &reader{t: t, w: w, dirs: dirs, onProgress: func(n int64) {
			done += n
			t.progress(done, total)
		}}
		n, err := r.read(ctx)
		if err == nil || !errors.Is(err, ErrBusy) {
			bad[w.file.Path] = n
		}
		if err != nil && !errors.Is(err, ErrBusy) && ctx.Err() == nil {
			agent := w.src.Agent()
			info := infos[agent]
			if info.Problem == nil {
				info.Problem = problemOf(readFailed(string(agent), w.file.Path, err))
			}
			infos[agent] = info
			t.logger().Warn("usage: reading records failed", "agent", agent, "code", CodeReadFailed, "err", err)
		}
	}
	for path, agent := range owner {
		info := infos[agent]
		info.Bad += bad[path]
		infos[agent] = info
	}
	return infos, true
}

// cost 是讀這個檔案要讀的位元組數（磁碟上），用來算進度。
func (w work) cost() int64 {
	if w.file.Format == Lines && w.stored != nil && w.stored.ReadTo <= w.stat.size {
		return w.stat.size - w.stored.ReadTo
	}
	return w.stat.size
}

// statFile 回傳檔案（連同 Watch）的大小之和與最晚的修改時間。
func statFile(f File) (fileStat, error) {
	fi, err := os.Stat(f.Path)
	if err != nil {
		return fileStat{}, err
	}
	st := fileStat{size: fi.Size(), modTime: fi.ModTime()}
	for _, p := range f.Watch {
		if wi, err := os.Stat(p); err == nil {
			st.size += wi.Size()
			if wi.ModTime().After(st.modTime) {
				st.modTime = wi.ModTime()
			}
		}
	}
	return st, nil
}

// reader 讀一個檔案變了的部分，一段一段地寫入。
type reader struct {
	t          *Tracker
	w          work
	dirs       map[string]string
	onProgress func(n int64)

	recs   []Record
	quotas []Quota
	bad    int
}

// read 讀檔案並回傳這份內容裡沒讀懂的行數。
func (r *reader) read(ctx context.Context) (int, error) {
	s := r.w.stored
	switch r.w.file.Format {
	case Database:
		return r.readDatabase(ctx)
	case Whole:
		return r.readWhole(ctx)
	case Compressed:
		var state []byte
		var from int64
		bad := 0
		// 讀到一半（崩潰或關閉）而檔案沒變：接著讀；變了就從頭解壓。
		if s != nil && !s.Done && s.Size == r.w.stat.size && s.ModTime.Equal(r.w.stat.modTime) {
			state, from, bad = s.State, s.ReadTo, s.Bad
		}
		return r.readLines(ctx, from, state, bad, true)
	default:
		var state []byte
		var from int64
		bad := 0
		if s != nil && s.ReadTo <= r.w.stat.size {
			head, err := fingerprint(r.w.file.Path, min(headBytes, s.ReadTo))
			if err != nil {
				return s.Bad, err
			}
			if head == s.Head {
				state, from, bad = s.State, s.ReadTo, s.Bad
			}
		}
		return r.readLines(ctx, from, state, bad, false)
	}
}

func (r *reader) readLines(ctx context.Context, from int64, state []byte, bad int, compressed bool) (int, error) {
	r.bad = bad
	p, err := r.w.src.Parser(r.w.file, state)
	if err != nil {
		return r.bad, err
	}
	f, err := os.Open(r.w.file.Path)
	if err != nil {
		return r.bad, err
	}
	defer f.Close()
	counter := &countingReader{r: f}
	var src io.Reader = counter
	if compressed {
		dec, err := zstd.NewReader(counter, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
		if err != nil {
			return r.bad, err
		}
		defer dec.Close()
		src = dec
		if from > 0 {
			if _, err := io.CopyN(io.Discard, dec, from); err != nil {
				return r.bad, err
			}
		}
	} else if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return r.bad, err
		}
	}
	br := bufio.NewReaderSize(src, 1<<20)
	pos := from
	chunkStart := pos
	reported := int64(0)
	report := func() {
		n := counter.n - reported
		if compressed {
			n = min(counter.n, r.w.stat.size) - reported
		}
		if n > 0 {
			reported += n
			r.onProgress(n)
		}
	}
	commit := func(done bool) error {
		fs := &FileState{Path: r.w.file.Path, Agent: r.w.src.Agent(), Size: r.w.stat.size,
			ModTime: r.w.stat.modTime, ReadTo: pos, State: p.State(), Bad: r.bad, Done: done}
		if !compressed {
			head, err := fingerprint(r.w.file.Path, min(headBytes, pos))
			if err != nil {
				return err
			}
			fs.Head = head
		}
		if err := r.flush(ctx, fs); err != nil {
			return err
		}
		chunkStart = pos
		report()
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return r.bad, err
		}
		line, complete, err := readLine(br)
		if err != nil && err != io.EOF {
			return r.bad, err
		}
		if len(line) > 0 && (complete || compressed) {
			pos += int64(len(line))
			if complete {
				pos++
			}
			r.parse(p, bytes.TrimSuffix(line, []byte("\r")))
		}
		if err == io.EOF {
			break
		}
		if len(r.recs) >= r.t.o.ChunkRecords || pos-chunkStart >= r.t.o.ChunkBytes {
			if err := commit(false); err != nil {
				return r.bad, err
			}
		}
	}
	return r.bad, commit(true)
}

func (r *reader) readWhole(ctx context.Context) (int, error) {
	r.bad = 0
	p, err := r.w.src.Parser(r.w.file, nil)
	if err != nil {
		return 0, err
	}
	content, err := os.ReadFile(r.w.file.Path)
	if err != nil {
		return 0, err
	}
	r.parse(p, content)
	fs := &FileState{Path: r.w.file.Path, Agent: r.w.src.Agent(), Size: r.w.stat.size,
		ModTime: r.w.stat.modTime, ReadTo: int64(len(content)), State: p.State(), Bad: r.bad, Done: true}
	if err := r.flush(ctx, fs); err != nil {
		return r.bad, err
	}
	r.onProgress(r.w.stat.size)
	return r.bad, nil
}

func (r *reader) readDatabase(ctx context.Context) (int, error) {
	db, ok := r.w.src.(DatabaseSource)
	if !ok {
		return 0, errors.New("usage: source does not read databases")
	}
	var state []byte
	if s := r.w.stored; s != nil {
		state = s.State
	}
	for {
		recs, next, more, err := db.ReadDatabase(ctx, r.w.file, state)
		if err != nil {
			return 0, err
		}
		r.recs = append(r.recs, r.fix(recs)...)
		state = next
		fs := &FileState{Path: r.w.file.Path, Agent: r.w.src.Agent(), Size: r.w.stat.size,
			ModTime: r.w.stat.modTime, State: state, Done: !more}
		if err := r.flush(ctx, fs); err != nil {
			return 0, err
		}
		if !more {
			r.onProgress(r.w.stat.size)
			return 0, nil
		}
	}
}

// parse 解析一行，收下它的用量與額度。
func (r *reader) parse(p Parser, line []byte) {
	recs, quotas, bad := p.Line(line)
	if bad {
		r.bad++
	}
	r.recs = append(r.recs, r.fix(recs)...)
	for _, q := range quotas {
		q.Agent = r.w.src.Agent()
		r.quotas = append(r.quotas, q)
	}
}

// fix 補上 agent、解析目錄，丟掉沒有 token 或沒有 Key 的用量。
func (r *reader) fix(recs []Record) []Record {
	out := recs[:0]
	for _, rec := range recs {
		if rec.Key == "" || rec.Tokens.Total() <= 0 || !rec.Tokens.Valid() {
			continue
		}
		rec.Agent = r.w.src.Agent()
		rec.Dir = r.realDir(rec.Dir)
		out = append(out, rec)
	}
	return out
}

// realDir 解開符號連結，解析不了時（目錄已不在）按原樣（D72、U19）。
func (r *reader) realDir(dir string) string {
	if dir == "" {
		return ""
	}
	if real, ok := r.dirs[dir]; ok {
		return real
	}
	real := dir
	if p, err := filepath.EvalSymlinks(dir); err == nil {
		real = p
	}
	r.dirs[dir] = real
	return real
}

// flush 把收下的用量與額度連同讀取位置寫入一個事務。
func (r *reader) flush(ctx context.Context, fs *FileState) error {
	err := r.t.o.Store.ApplyUsage(ctx, Batch{Records: r.recs, Quotas: r.quotas, File: fs})
	r.recs, r.quotas = r.recs[:0], r.quotas[:0]
	return err
}

// readLine 讀一行完整的行，不含換行；complete 表示讀到了換行。行沒有長度上限。
func readLine(br *bufio.Reader) (line []byte, complete bool, err error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			if buf == nil {
				return chunk[:len(chunk)-1], true, nil
			}
			return append(buf, chunk[:len(chunk)-1]...), true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			buf = append(buf, chunk...)
		default:
			return append(buf, chunk...), false, err
		}
	}
}

// fingerprint 是檔案開頭 n 個位元組的雜湊。
func fingerprint(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil && err != io.EOF {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// problemOf 把錯誤轉成記錄來源裡的原因（代碼加參數）。
func problemOf(e *Error) *task.Problem {
	return &task.Problem{Code: string(e.Code), Params: e.Params}
}

// sortedAgents 依 Sources 的順序回傳 agent。
func (t *Tracker) sortedAgents() []task.AgentID {
	out := make([]task.AgentID, 0, len(t.o.Sources))
	for _, s := range t.o.Sources {
		if !slices.Contains(out, s.Agent()) {
			out = append(out, s.Agent())
		}
	}
	return out
}
