package updater

// notes.go：更新後第一次啟動顯示這個版本的更新說明（m2.md 第 27 節 RN01–RN13，D142）。
// 更新說明是每個 Release 上的 release-notes.json，由 app 注入拉取的函式；
// 上次執行的版本與這個版本的說明顯示過沒有，記在資料目錄的一個小檔案裡。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ReleaseNotes 是一個版本的更新說明：Release 上的 release-notes.json。
type ReleaseNotes struct {
	// Version 是說明所屬的版本（x.y.z，不帶 v）。
	Version string `json:"version"`
	// Notes 是語言標記（如 en、zh-Hans）對 Markdown 正文；頁面取界面語言的一份，
	// 沒有就用 en。
	Notes map[string]string `json:"notes"`
}

// NotesOptions 是 Notes 的依賴；mygo 的能力由 app 注入（app.NewReleaseNotes）。
type NotesOptions struct {
	// Current 是正在執行的版本。
	Current string
	// Record 是記錄檔的路徑（資料目錄裡），記上次執行的版本與它的說明顯示過沒有。
	Record string
	// Enabled 回答這個構建有沒有更新源，在背景問一次；nil 或回答否（本機構建、
	// 開發構建）時不拉取也不顯示（RN08）。
	Enabled func() bool
	// Fetch 取 version 的 release-notes.json 的內容。Enabled 為真時必填。
	Fetch func(ctx context.Context, version string) ([]byte, error)
	// Timeout 是一次拉取的時限；0 → 40 秒（RN05）：夠先等 GitHub、再等備用源各 15 秒（US08）。
	Timeout time.Duration
	// Logger 記錄拉取與記錄檔的失敗；nil → 記錄時的 slog.Default()。
	Logger *slog.Logger
}

// notesTimeout 是一次拉取的預設時限。
const notesTimeout = 40 * time.Second

// MaxReleaseNotes 是 release-notes.json 最多多少位元組；更大的不顯示。
const MaxReleaseNotes = 1 << 20

// Notes 決定這次啟動要不要顯示更新說明，要的話拉取並交給頁面；頁面顯示了就記下，
// 每個版本只顯示一次。方法可從任何 goroutine 呼叫。
type Notes struct {
	o      NotesOptions
	ctx    context.Context // 拉取用；Close 取消
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	pending *ReleaseNotes // 拉到了、還沒顯示的說明；沒有則 nil
	changed chan struct{} // pending 每次變化關閉並換新
	started bool
}

// notesRecord 是記錄檔的內容。
type notesRecord struct {
	// Version 是上次執行的版本。
	Version string `json:"version"`
	// Pending 表示 Version 是更新來的，它的說明還沒顯示過（拉取失敗時下次啟動再試，RN05）。
	Pending bool `json:"pending,omitzero"`
}

// NewNotes 只建構，不做 I/O；Start 才讀記錄檔。
func NewNotes(o NotesOptions) *Notes {
	if o.Timeout == 0 {
		o.Timeout = notesTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Notes{o: o, ctx: ctx, cancel: cancel, changed: make(chan struct{})}
}

// Start 在背景讀記錄檔，是更新後第一次顯示時拉取說明；只有第一次呼叫有作用。
func (n *Notes) Start() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return
	}
	n.started = true
	n.wg.Go(n.run)
}

// Close 取消進行中的拉取並等它結束。
func (n *Notes) Close() {
	n.cancel()
	n.wg.Wait()
}

func (n *Notes) logger() *slog.Logger {
	if n.o.Logger != nil {
		return n.o.Logger
	}
	return slog.Default()
}

// run 判斷這次要不要顯示（RN01–RN08、RN13），要的話拉取。
func (n *Notes) run() {
	if !n.due() {
		return
	}
	notes, err := n.fetch()
	if err != nil {
		// 不顯示；記錄檔仍是 pending，下次啟動再試（RN05）。
		if n.ctx.Err() == nil {
			n.logger().Warn("release notes unavailable", "version", n.o.Current, "err", err)
		}
		return
	}
	if notes.empty() {
		n.settle() // RN06
		return
	}
	n.mu.Lock()
	n.pending = notes
	n.notifyLocked()
	n.mu.Unlock()
}

// due 讀記錄檔、寫下這次的版本，回答這次要不要拉取說明。
func (n *Notes) due() bool {
	rec, err := n.read()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// 第一次安裝（RN01）。
		n.write(notesRecord{Version: n.o.Current})
		return false
	case err != nil:
		// 讀不了（RN13）：不顯示，換成這次的版本。
		n.logger().Warn("release notes record unreadable", "err", err)
		n.write(notesRecord{Version: n.o.Current})
		return false
	}
	pending := rec.Pending && rec.Version == n.o.Current
	if rec.Version != n.o.Current {
		pending = newer(n.o.Current, rec.Version) // 降級不顯示（RN07）
	}
	if pending && (n.o.Enabled == nil || !n.o.Enabled()) {
		pending = false // 沒有更新源（RN08）
	}
	if rec.Version != n.o.Current || rec.Pending != pending {
		// 寫不了時不顯示，否則每次啟動都會再顯示（RN13）。
		if !n.write(notesRecord{Version: n.o.Current, Pending: pending}) {
			return false
		}
	}
	return pending
}

// fetch 拉取並檢查這個版本的說明（RN05）。
func (n *Notes) fetch() (*ReleaseNotes, error) {
	ctx, cancel := context.WithTimeout(n.ctx, n.o.Timeout)
	defer cancel()
	data, err := n.o.Fetch(ctx, n.o.Current)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxReleaseNotes {
		return nil, fmt.Errorf("release notes are larger than %d bytes", MaxReleaseNotes)
	}
	var notes ReleaseNotes
	if err := json.Unmarshal(data, &notes); err != nil {
		return nil, fmt.Errorf("release notes: %w", err)
	}
	if strings.TrimPrefix(notes.Version, "v") != n.o.Current {
		return nil, fmt.Errorf("release notes are for %q, not %q", notes.Version, n.o.Current)
	}
	notes.Version = n.o.Current
	return &notes, nil
}

// empty 回答說明是不是每一種語言都沒有內容。
func (r *ReleaseNotes) empty() bool {
	for _, text := range r.Notes {
		if strings.TrimSpace(text) != "" {
			return false
		}
	}
	return true
}

// Watch 在有該顯示的說明時以它呼叫 send（每次 Watch 最多一次）；ctx 結束時回傳。
// 說明在 Watch 之後才拉到也會送出（RN09）；send 的錯誤原樣回傳。
func (n *Notes) Watch(ctx context.Context, send func(ReleaseNotes) error) error {
	for {
		n.mu.Lock()
		pending, changed := n.pending, n.changed
		n.mu.Unlock()
		if pending != nil {
			if err := send(*pending); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}
	}
}

// Shown 記下 version 的說明已顯示（RN03）：之後的 Watch 不再送出，下次啟動也不顯示。
// version 不是待顯示的版本時不做什麼。
func (n *Notes) Shown(version string) {
	n.mu.Lock()
	if n.pending == nil || n.pending.Version != version {
		n.mu.Unlock()
		return
	}
	n.pending = nil
	n.notifyLocked()
	n.mu.Unlock()
	n.settle()
}

// settle 把這個版本記為不必再顯示。
func (n *Notes) settle() {
	n.write(notesRecord{Version: n.o.Current})
}

func (n *Notes) notifyLocked() {
	close(n.changed)
	n.changed = make(chan struct{})
}

func (n *Notes) read() (notesRecord, error) {
	data, err := os.ReadFile(n.o.Record)
	if err != nil {
		return notesRecord{}, err
	}
	var rec notesRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return notesRecord{}, err
	}
	return rec, nil
}

// write 寫記錄檔（先寫暫存檔再換名），回答寫成了沒有；失敗只記日誌（RN13）。
func (n *Notes) write(rec notesRecord) bool {
	data, err := json.Marshal(rec)
	if err == nil {
		tmp := n.o.Record + ".tmp"
		if err = os.MkdirAll(filepath.Dir(n.o.Record), 0o700); err == nil {
			if err = os.WriteFile(tmp, data, 0o600); err == nil {
				err = os.Rename(tmp, n.o.Record)
			}
		}
	}
	if err != nil {
		n.logger().Warn("release notes record not saved", "err", err)
		return false
	}
	return true
}

// newer 回答版本 a 是否比 b 新（x.y.z）；任一個不是 x.y.z 時回 false。
func newer(a, b string) bool {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	if !okA || !okB {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
