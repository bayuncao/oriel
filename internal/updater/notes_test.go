package updater

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// notesFetch 是記下呼叫的 Fetch：回 body 或 err。
type notesFetch struct {
	mu    sync.Mutex
	calls []string
	body  string
	err   error
	// block 為真時等到 ctx 結束才回傳（拉取逾時）。
	block bool
}

func (f *notesFetch) fetch(ctx context.Context, version string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, version)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []byte(f.body), f.err
}

func (f *notesFetch) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// enabled 是有更新源的構建。
func enabled() bool { return true }

const notes015 = `{"version":"0.1.5","notes":{"en":"## New\n- Split bar","zh-Hans":"## 新功能\n- 色条"}}`

// runNotes 建 Notes、Start 並等背景判斷與拉取結束，回傳它。
func runNotes(t *testing.T, o NotesOptions) *Notes {
	t.Helper()
	n := NewNotes(o)
	n.Start()
	n.wg.Wait()
	t.Cleanup(n.Close)
	return n
}

// pendingNotes 回傳待顯示的說明（沒有則 nil）。
func pendingNotes(n *Notes) *ReleaseNotes {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pending
}

func readRecord(t *testing.T, path string) notesRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rec notesRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("record %q: %v", data, err)
	}
	return rec
}

func writeRecord(t *testing.T, path string, rec notesRecord) {
	t.Helper()
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNotesStart(t *testing.T) {
	cases := []struct {
		name    string
		record  *notesRecord // nil：沒有記錄檔
		raw     string       // 不為空時直接寫進記錄檔
		current string
		fetch   *notesFetch // nil：沒有更新源
		// 期望
		fetched bool
		shows   bool
		after   notesRecord
	}{
		{name: "RN01 first install", current: "0.1.5", fetch: &notesFetch{body: notes015},
			after: notesRecord{Version: "0.1.5"}},
		{name: "RN02 updated", record: &notesRecord{Version: "0.1.4"}, current: "0.1.5", fetch: &notesFetch{body: notes015},
			fetched: true, shows: true, after: notesRecord{Version: "0.1.5", Pending: true}},
		{name: "RN03 already shown", record: &notesRecord{Version: "0.1.5"}, current: "0.1.5", fetch: &notesFetch{body: notes015},
			after: notesRecord{Version: "0.1.5"}},
		{name: "RN04 skipped versions show only this one", record: &notesRecord{Version: "0.1.2"}, current: "0.1.5", fetch: &notesFetch{body: notes015},
			fetched: true, shows: true, after: notesRecord{Version: "0.1.5", Pending: true}},
		{name: "RN05 fetch failed stays pending", record: &notesRecord{Version: "0.1.4"}, current: "0.1.5", fetch: &notesFetch{err: errors.New("offline")},
			fetched: true, after: notesRecord{Version: "0.1.5", Pending: true}},
		{name: "RN05 retried on the next launch", record: &notesRecord{Version: "0.1.5", Pending: true}, current: "0.1.5", fetch: &notesFetch{body: notes015},
			fetched: true, shows: true, after: notesRecord{Version: "0.1.5", Pending: true}},
		{name: "RN05 not JSON", record: &notesRecord{Version: "0.1.4"}, current: "0.1.5", fetch: &notesFetch{body: "<html>"},
			fetched: true, after: notesRecord{Version: "0.1.5", Pending: true}},
		{name: "RN05 another version", record: &notesRecord{Version: "0.1.4"}, current: "0.1.5", fetch: &notesFetch{body: `{"version":"0.1.4","notes":{"en":"old"}}`},
			fetched: true, after: notesRecord{Version: "0.1.5", Pending: true}},
		{name: "RN05 v prefix is the same version", record: &notesRecord{Version: "0.1.4"}, current: "0.1.5", fetch: &notesFetch{body: `{"version":"v0.1.5","notes":{"en":"x"}}`},
			fetched: true, shows: true, after: notesRecord{Version: "0.1.5", Pending: true}},
		{name: "RN06 empty notes", record: &notesRecord{Version: "0.1.4"}, current: "0.1.5", fetch: &notesFetch{body: `{"version":"0.1.5","notes":{"en":"  ","zh-Hans":""}}`},
			fetched: true, after: notesRecord{Version: "0.1.5"}},
		{name: "RN07 downgrade", record: &notesRecord{Version: "0.2.0"}, current: "0.1.5", fetch: &notesFetch{body: notes015},
			after: notesRecord{Version: "0.1.5"}},
		{name: "RN07 10 is newer than 9", record: &notesRecord{Version: "0.1.9"}, current: "0.1.10", fetch: &notesFetch{body: `{"version":"0.1.10","notes":{"en":"x"}}`},
			fetched: true, shows: true, after: notesRecord{Version: "0.1.10", Pending: true}},
		{name: "RN08 no update source", record: &notesRecord{Version: "0.1.4"}, current: "0.1.5",
			after: notesRecord{Version: "0.1.5"}},
		{name: "RN13 unreadable record", raw: "{not json", current: "0.1.5", fetch: &notesFetch{body: notes015},
			after: notesRecord{Version: "0.1.5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "release-notes-seen.json")
			if tc.record != nil {
				writeRecord(t, path, *tc.record)
			}
			if tc.raw != "" {
				if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			o := NotesOptions{Current: tc.current, Record: path}
			if tc.fetch != nil {
				o.Enabled, o.Fetch = enabled, tc.fetch.fetch
			}
			n := runNotes(t, o)
			if got := tc.fetch != nil && tc.fetch.count() > 0; got != tc.fetched {
				t.Errorf("fetched = %v, want %v", got, tc.fetched)
			}
			if got := pendingNotes(n) != nil; got != tc.shows {
				t.Errorf("shows = %v, want %v", got, tc.shows)
			}
			if got := readRecord(t, path); got != tc.after {
				t.Errorf("record = %+v, want %+v", got, tc.after)
			}
		})
	}
}

// RN03：顯示了就記下，之後的 Watch 不再送，下次啟動也不顯示。
func TestNotesShownOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-notes-seen.json")
	writeRecord(t, path, notesRecord{Version: "0.1.4"})
	fetch := &notesFetch{body: notes015}
	n := runNotes(t, NotesOptions{Current: "0.1.5", Record: path, Enabled: enabled, Fetch: fetch.fetch})

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan ReleaseNotes, 1)
	done := make(chan error, 1)
	go func() { done <- n.Watch(ctx, func(r ReleaseNotes) error { got <- r; return nil }) }()
	r := <-got
	if r.Version != "0.1.5" || r.Notes["zh-Hans"] == "" || r.Notes["en"] == "" {
		t.Fatalf("notes = %+v", r)
	}
	n.Shown("0.1.5")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if rec := readRecord(t, path); rec != (notesRecord{Version: "0.1.5"}) {
		t.Errorf("record after shown = %+v", rec)
	}

	// 同一次執行裡再看：不送。
	ctx2, cancel2 := context.WithCancel(context.Background())
	sent := false
	done2 := make(chan error, 1)
	go func() { done2 <- n.Watch(ctx2, func(ReleaseNotes) error { sent = true; return nil }) }()
	cancel2()
	<-done2
	if sent {
		t.Error("notes sent again after Shown")
	}

	// 下次啟動：不拉取、不顯示。
	again := &notesFetch{body: notes015}
	n2 := runNotes(t, NotesOptions{Current: "0.1.5", Record: path, Enabled: enabled, Fetch: again.fetch})
	if again.count() != 0 || pendingNotes(n2) != nil {
		t.Errorf("next launch fetched %d times, pending %v", again.count(), pendingNotes(n2))
	}
}

// RN09：說明在 Watch 之後才拉到也會送出。
func TestNotesWatchBeforeFetched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-notes-seen.json")
	writeRecord(t, path, notesRecord{Version: "0.1.4"})
	release := make(chan struct{})
	n := NewNotes(NotesOptions{Current: "0.1.5", Record: path, Enabled: enabled, Fetch: func(ctx context.Context, _ string) ([]byte, error) {
		<-release
		return []byte(notes015), nil
	}})
	t.Cleanup(n.Close)

	got := make(chan ReleaseNotes, 1)
	go func() { _ = n.Watch(t.Context(), func(r ReleaseNotes) error { got <- r; return nil }) }()
	n.Start()
	close(release)
	if r := <-got; r.Version != "0.1.5" {
		t.Fatalf("notes = %+v", r)
	}
}

// RN05：拉取逾時不顯示，仍待下次啟動再試。
func TestNotesFetchTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-notes-seen.json")
	writeRecord(t, path, notesRecord{Version: "0.1.4"})
	fetch := &notesFetch{block: true}
	n := runNotes(t, NotesOptions{Current: "0.1.5", Record: path, Enabled: enabled, Fetch: fetch.fetch, Timeout: time.Millisecond})
	if pendingNotes(n) != nil {
		t.Error("timed-out fetch shows notes")
	}
	if rec := readRecord(t, path); rec != (notesRecord{Version: "0.1.5", Pending: true}) {
		t.Errorf("record = %+v", rec)
	}
}

// RN13：記錄檔寫不了時不拉取、不顯示（否則每次啟動都會再顯示）。
func TestNotesRecordNotWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "release-notes-seen.json")
	writeRecord(t, path, notesRecord{Version: "0.1.4"})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	fetch := &notesFetch{body: notes015}
	n := runNotes(t, NotesOptions{Current: "0.1.5", Record: path, Enabled: enabled, Fetch: fetch.fetch})
	if fetch.count() != 0 || pendingNotes(n) != nil {
		t.Errorf("fetched %d times, pending %v", fetch.count(), pendingNotes(n))
	}
}

// Shown 只認待顯示的版本。
func TestNotesShownOtherVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-notes-seen.json")
	writeRecord(t, path, notesRecord{Version: "0.1.4"})
	fetch := &notesFetch{body: notes015}
	n := runNotes(t, NotesOptions{Current: "0.1.5", Record: path, Enabled: enabled, Fetch: fetch.fetch})
	n.Shown("0.1.4")
	if pendingNotes(n) == nil {
		t.Error("Shown of another version cleared the notes")
	}
}

// Close 取消進行中的拉取並等它結束。
func TestNotesCloseCancelsFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-notes-seen.json")
	writeRecord(t, path, notesRecord{Version: "0.1.4"})
	started := make(chan struct{})
	n := NewNotes(NotesOptions{Current: "0.1.5", Record: path, Timeout: time.Hour, Enabled: enabled, Fetch: func(ctx context.Context, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	n.Start()
	<-started
	n.Close()
	if pendingNotes(n) != nil {
		t.Error("cancelled fetch shows notes")
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.5", "0.1.4", true},
		{"0.1.10", "0.1.9", true},
		{"1.0.0", "0.9.9", true},
		{"0.1.4", "0.1.4", false},
		{"0.1.4", "0.1.5", false},
		{"v0.1.5", "0.1.4", true},
		{"0.1", "0.0.1", false},
		{"0.1.5", "dev", false},
		{"0.1.x", "0.1.4", false},
	}
	for _, tc := range cases {
		if got := newer(tc.a, tc.b); got != tc.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
