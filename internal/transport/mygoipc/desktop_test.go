package mygoipc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/transport"
	"github.com/bayuncao/oriel/internal/updater"
)

// startUpdates 回傳啟動了的、能自我更新的 updater，檢查的結果是 check 的。
func startUpdates(t *testing.T, check func(context.Context) (*updater.Release, error)) *updater.Updater {
	t.Helper()
	u := updater.New(updater.Options{Enabled: func() bool { return true }, Check: check,
		Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(u.Close)
	u.Start()
	return u
}

// connectUpdates 給 Desktop 接上 startUpdates 的 updater。
func connectUpdates(t *testing.T, check func(context.Context) (*updater.Release, error)) *Desktop {
	t.Helper()
	s := &Desktop{}
	ConnectDesktop(s, DesktopParts{Updates: startUpdates(t, check)})
	return s
}

// D3：Desktop 在 App.Run 之後才接上（WhenReady）。接上之前回 not_ready；與頁面
// 的呼叫同時接上不會競爭（-race），那次呼叫回 not_ready 或結果；之後用接上的 updater。
func TestConnectDesktopLate(t *testing.T) {
	u := startUpdates(t, func(context.Context) (*updater.Release, error) { return nil, nil })
	notReady := func(err error) bool {
		var we *transport.Error
		return errors.As(err, &we) && we.Code == string(CodeNotReady)
	}
	s := &Desktop{}
	if _, err := s.CheckForUpdates(ctxb()); !notReady(err) {
		t.Fatalf("before connecting: %v", err)
	}
	during := make(chan error)
	go func() {
		_, err := s.CheckForUpdates(ctxb())
		during <- err
	}()
	ConnectDesktop(s, DesktopParts{Updates: u})
	if err := <-during; err != nil && !notReady(err) {
		t.Fatalf("while connecting: %v", err)
	}
	if st, err := s.CheckForUpdates(ctxb()); err != nil || st.Phase != updater.Idle {
		t.Fatalf("after connecting: %+v %v", st, err)
	}
}

// mainThread 扮演 mygo 的主線程：一個 goroutine 依序執行交來的函式；run 等
// 函式做完才返回，與 mygo.RunOnMain 相同。
type mainThread chan func()

// startMainThread 啟動測試的主線程，測試結束時停下。
func startMainThread(t *testing.T) mainThread {
	t.Helper()
	m := make(mainThread)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for fn := range m {
			fn()
		}
	}()
	t.Cleanup(func() {
		close(m)
		<-stopped
	})
	return m
}

func (m mainThread) run(fn func()) {
	done := make(chan struct{})
	m <- func() {
		defer close(done)
		fn()
	}
	<-done
}

// D4：頁面的 ShowMain 跑在各自的 goroutine 上，打開主視窗交給主線程，做完才
// 返回。主視窗關著時，兩個面板的呼叫與主線程上的一次打開（如 OnActivate）同時
// 來，只開一個；show 只在主線程上執行，所以 -race 下讀寫 open 不競爭。
func TestShowMainOnMainThread(t *testing.T) {
	mt := startMainThread(t)
	open, opened := false, 0 // 只在主線程上讀寫，同 main.go 的 win
	show := func() {
		if !open {
			open = true
			opened++
		}
	}
	s := &Desktop{onMain: mt.run}
	ConnectDesktop(s, DesktopParts{Show: show})

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { s.ShowMain(ctxb()) })
	}
	mt.run(show)
	wg.Wait()
	if opened != 1 {
		t.Fatalf("opened %d windows at once", opened)
	}

	// 關掉主視窗（main.go 的 OnClosed，也在主線程上）再打開：ShowMain 返回時已重新打開。
	mt.run(func() { open = false })
	s.ShowMain(ctxb())
	if opened != 2 {
		t.Fatalf("after ShowMain returned: opened %d", opened)
	}
}

// M17：ShowTask 先打開主視窗（在主線程上做完），再廣播 open:task。
func TestShowTask(t *testing.T) {
	mt := startMainThread(t)
	var order []string // 主線程與呼叫者依序寫入：show 在 ShowMain 返回前做完
	s := &Desktop{onMain: mt.run, openTask: func(ev OpenTaskEvent) { order = append(order, "open "+string(ev.Task)) }}
	ConnectDesktop(s, DesktopParts{Show: func() { order = append(order, "show") }})
	s.ShowTask(ctxb(), "t-1")
	if want := []string{"show", "open t-1"}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
}

// Q15/Q16-desktop：搜尋面板的最近搜尋存在記憶體裡（D67），關掉主視窗、頁面重新載入後還在，
// 退出應用就沒了。存什麼取回什麼，新的在前；去掉首尾空白與空的，最多 3 條；取回的是複本。
func TestRecentSearches(t *testing.T) {
	for _, tc := range []struct {
		name      string
		set, want []string
	}{
		{"nothing yet", nil, nil},
		{"kept in order", []string{"oauth", "登录"}, []string{"oauth", "登录"}},
		{"trimmed, blanks dropped", []string{"  登录 ", "", "   ", "a b"}, []string{"登录", "a b"}},
		{"at most three", []string{"a", "b", "c", "d"}, []string{"a", "b", "c"}},
		{"three after blanks", []string{"", "a", " ", "b", "c", "d"}, []string{"a", "b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Desktop{}
			if tc.set != nil {
				s.SetRecentSearches(tc.set)
			}
			got := s.RecentSearches()
			if !slices.Equal(got, tc.want) {
				t.Fatalf("recent %q, want %q", got, tc.want)
			}
			if len(got) > 0 {
				got[0] = "changed"
				if s.RecentSearches()[0] == "changed" {
					t.Fatal("the returned slice shares storage with Desktop")
				}
			}
		})
	}
}

// 頁面的呼叫跑在各自的 goroutine 上：同時存取最近搜尋不競爭（-race）。
func TestRecentSearchesConcurrent(t *testing.T) {
	s := &Desktop{}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() { s.SetRecentSearches([]string{fmt.Sprint(i)}) })
		wg.Go(func() { _ = s.RecentSearches() })
	}
	wg.Wait()
	if got := s.RecentSearches(); len(got) != 1 {
		t.Fatalf("recent %q, want one search", got)
	}
}

// T18、T33：拖進來的只有路徑；Folder 只對存在的資料夾（含指向它的符號連結）回傳
// 解開符號連結後的路徑，其他回傳 ""。
func TestFolder(t *testing.T) {
	// 暫存目錄本身可能在符號連結下（macOS 的 /var → /private/var），期望值一律取真實路徑。
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(dir, "folder")
	file := filepath.Join(dir, "file.txt")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, link := range [][2]string{{folder, "to-folder"}, {file, "to-file"}, {filepath.Join(dir, "gone"), "dangling"}} {
		if err := os.Symlink(link[0], filepath.Join(dir, link[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(folder, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(mustGetwd(t), folder)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		want       string
	}{
		{"folder", folder, folder},
		{"folder with trailing slash", folder + "/", folder},
		{"link to a folder", filepath.Join(dir, "to-folder"), folder},
		{"folder under a link", filepath.Join(dir, "to-folder", "sub"), filepath.Join(folder, "sub")},
		{"file", file, ""},
		{"link to a file", filepath.Join(dir, "to-file"), ""},
		{"dangling link", filepath.Join(dir, "dangling"), ""},
		{"missing", filepath.Join(dir, "missing"), ""},
		{"relative path to a folder", rel, ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&Desktop{}).Folder(tc.path); got != tc.want {
				t.Fatalf("Folder(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// D1：CheckForUpdates 回傳結束時的狀態；失敗的代碼與參數經 Wire 到頁面。
func TestCheckForUpdates(t *testing.T) {
	s := connectUpdates(t, func(context.Context) (*updater.Release, error) { return nil, nil })
	if st, err := s.CheckForUpdates(ctxb()); err != nil || st.Phase != updater.Idle {
		t.Fatalf("up to date: %+v %v", st, err)
	}

	s = connectUpdates(t, func(context.Context) (*updater.Release, error) {
		return nil, errors.New("offline")
	})
	_, err := s.CheckForUpdates(ctxb())
	var we *transport.Error
	if !errors.As(err, &we) || we.Code != string(updater.CodeCheckFailed) || we.Params["detail"] != "offline" {
		t.Fatalf("failed check: %v", err)
	}
}

// D2：頁面關掉 channel（Send 回 ErrChannelClosed）時 watch 正常結束，不當成錯誤。
func TestWatchUpdatesPageLeft(t *testing.T) {
	s := connectUpdates(t, func(context.Context) (*updater.Release, error) { return nil, nil })
	var sent []updater.Status
	err := pageLeft(ctxb(), s.connected().Updates.Watch(ctxb(), func(st updater.Status) error {
		sent = append(sent, st)
		return mygo.ErrChannelClosed
	}))
	if err != nil || len(sent) != 1 || sent[0].Phase != updater.Idle {
		t.Fatalf("sent %+v, err %v", sent, err)
	}
}

// RN02、RN03：拉到的更新說明推一次；頁面關掉 channel 時正常結束；接上之前回 not_ready；
// ReleaseNotesShown 之後不再推。
func TestWatchReleaseNotes(t *testing.T) {
	s := &Desktop{}
	var we *transport.Error
	if err := s.WatchReleaseNotes(ctxb(), nil); !errors.As(err, &we) || we.Code != string(CodeNotReady) {
		t.Fatalf("before connecting: %v", err)
	}
	s.ReleaseNotesShown("0.1.5") // 沒接上時什麼都不做

	dir := t.TempDir()
	record := filepath.Join(dir, "seen.json")
	if err := os.WriteFile(record, []byte(`{"version":"0.1.4"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	n := updater.NewNotes(updater.NotesOptions{
		Current: "0.1.5", Record: record, Enabled: func() bool { return true },
		Fetch: func(context.Context, string) ([]byte, error) {
			return []byte(`{"version":"0.1.5","notes":{"en":"New"}}`), nil
		},
	})
	t.Cleanup(n.Close)
	n.Start()
	ConnectDesktop(s, DesktopParts{Notes: n})

	var sent []updater.ReleaseNotes
	err := pageLeft(ctxb(), s.connected().Notes.Watch(ctxb(), func(r updater.ReleaseNotes) error {
		sent = append(sent, r)
		return mygo.ErrChannelClosed
	}))
	if err != nil || len(sent) != 1 || sent[0].Version != "0.1.5" {
		t.Fatalf("sent %+v, err %v", sent, err)
	}

	s.ReleaseNotesShown("0.1.5")
	ctx, cancel := context.WithCancel(ctxb())
	cancel()
	sent = nil
	if err := pageLeft(ctx, s.connected().Notes.Watch(ctx, func(r updater.ReleaseNotes) error {
		sent = append(sent, r)
		return nil
	})); err != nil || len(sent) != 0 {
		t.Fatalf("after shown: sent %+v, err %v", sent, err)
	}
}

// V10：OpenLink 只把 http、https 與 mailto 交給系統打開；其他協議、沒有主機的網址與打不開的字串回
// mygoipc.unsupported_link，不交給系統。系統打不開時原因經 Wire 回頁面。
func TestOpenLink(t *testing.T) {
	failed := errors.New("no browser")
	for _, tc := range []struct {
		name, link string
		opened     string // 交給系統的連結；"" 是沒有交
		openErr    error
		code       string
	}{
		{"https", "https://example.com/a?b=1#c", "https://example.com/a?b=1#c", nil, ""},
		{"http", "http://localhost:5173/", "http://localhost:5173/", nil, ""},
		{"mailto", "mailto:someone@example.com", "mailto:someone@example.com", nil, ""},
		{"scheme in capitals", "HTTPS://example.com", "https://example.com", nil, ""},
		{"file", "file:///etc/passwd", "", nil, string(CodeUnsupportedLink)},
		{"javascript", "javascript:alert(1)", "", nil, string(CodeUnsupportedLink)},
		{"relative", "/tasks/t-1", "", nil, string(CodeUnsupportedLink)},
		{"http without host", "http:///path", "", nil, string(CodeUnsupportedLink)},
		{"not a URL", "http://[::1", "", nil, string(CodeUnsupportedLink)},
		{"system fails", "https://example.com", "https://example.com", failed, string(transport.CodeInternal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opened string
			s := &Desktop{openExternal: func(link string) error {
				opened = link
				return tc.openErr
			}}
			err := s.OpenLink(tc.link)
			if opened != tc.opened {
				t.Fatalf("opened %q, want %q", opened, tc.opened)
			}
			if tc.code == "" {
				if err != nil {
					t.Fatalf("OpenLink: %v", err)
				}
				return
			}
			var we *transport.Error
			if !errors.As(err, &we) {
				t.Fatalf("err = %v, want a wire error", err)
			}
			if we.Code != tc.code {
				t.Fatalf("code = %q, want %q", we.Code, tc.code)
			}
		})
	}
}

// V44–V46：只有工作目錄之下的一般檔案能附，回傳解開符號連結後的路徑；資料夾、不存在的與相對路徑是
// not_file，工作目錄之外的（含工作目錄裡指向外面的符號連結、名字以 .. 開頭的兄弟目錄）是 outside。
func TestAttachable(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	sibling := filepath.Join(base, "work-other")
	for _, d := range []string{filepath.Join(work, "src"), sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string) string {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	inside := write(filepath.Join(work, "src", "a.ts"))
	outside := write(filepath.Join(base, "secret.txt"))
	other := write(filepath.Join(sibling, "b.ts"))
	dotted := write(filepath.Join(work, "..hidden"))
	for _, link := range [][2]string{
		{inside, filepath.Join(base, "to-inside")},
		{outside, filepath.Join(work, "to-outside")},
		{work, filepath.Join(base, "work-link")},
	} {
		if err := os.Symlink(link[0], link[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, dir, path string
		want            Attachable
	}{
		{"file in the working directory", work, inside, Attachable{Path: inside}},
		{"file named with two dots inside", work, dotted, Attachable{Path: dotted}},
		{"link outside to a file inside", work, filepath.Join(base, "to-inside"), Attachable{Path: inside}},
		{"working directory given through a link", filepath.Join(base, "work-link"), inside, Attachable{Path: inside}},
		{"file reached through the linked directory", work, filepath.Join(base, "work-link", "src", "a.ts"), Attachable{Path: inside}},
		{"file outside", work, outside, Attachable{Rejection: AttachOutside}},
		{"link inside to a file outside", work, filepath.Join(work, "to-outside"), Attachable{Rejection: AttachOutside}},
		{"sibling directory with the same prefix", work, other, Attachable{Rejection: AttachOutside}},
		{"folder", work, filepath.Join(work, "src"), Attachable{Rejection: AttachNotFile}},
		{"the working directory itself", work, work, Attachable{Rejection: AttachNotFile}},
		{"missing", work, filepath.Join(work, "gone.ts"), Attachable{Rejection: AttachNotFile}},
		{"relative path", work, "src/a.ts", Attachable{Rejection: AttachNotFile}},
		{"working directory missing", filepath.Join(base, "gone"), inside, Attachable{Rejection: AttachOutside}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&Desktop{}).Attachable(tc.dir, tc.path); got != tc.want {
				t.Fatalf("Attachable(%q, %q) = %+v, want %+v", tc.dir, tc.path, got, tc.want)
			}
		})
	}
}

// keepAwakeSetting 是記在記憶體裡的開關：fail 非 nil 時 Set 回它。
type keepAwakeSetting struct {
	on   bool
	fail error
}

func (k *keepAwakeSetting) On() bool { return k.on }
func (k *keepAwakeSetting) Set(on bool) error {
	if k.fail != nil {
		return k.fail
	}
	k.on = on
	return nil
}

// KA10、KA13、KA14：頁面經 Desktop 讀寫開關；寫不進去時原因帶著代碼回給頁面。
func TestDesktopKeepAwake(t *testing.T) {
	k := &keepAwakeSetting{on: true}
	s := &Desktop{}
	ConnectDesktop(s, DesktopParts{KeepAwake: k})
	if on, err := s.KeepAwake(); err != nil || !on {
		t.Fatalf("KeepAwake = %v, %v; want true", on, err)
	}
	if err := s.SetKeepAwake(false); err != nil || k.on {
		t.Fatalf("SetKeepAwake(false): %v, on = %v", err, k.on)
	}
	k.fail = &transport.Error{Code: "app.keep_awake_save_failed", Params: map[string]string{"detail": "read-only"}}
	var we *transport.Error
	if err := s.SetKeepAwake(true); !errors.As(err, &we) || we.Code != "app.keep_awake_save_failed" || we.Params["detail"] != "read-only" {
		t.Fatalf("SetKeepAwake failure = %v", err)
	}
	if on, _ := s.KeepAwake(); on {
		t.Fatal("a failed save changed the switch")
	}
}

type accountSetting struct {
	without bool
	fail    error
}

func (a *accountSetting) WithoutAccount() bool { return a.without }
func (a *accountSetting) ContinueWithout() error {
	a.without = true
	return a.fail
}

// NA01–NA05：接上之前不猜（not_ready）；頁面經 Desktop 讀與記“無賬號使用”；
// 寫不進去時這次照樣算選過，原因帶著代碼回給頁面。
func TestDesktopAccount(t *testing.T) {
	s := &Desktop{}
	var we *transport.Error
	if _, err := s.WithoutAccount(); !errors.As(err, &we) || we.Code != string(CodeNotReady) {
		t.Fatalf("WithoutAccount before connecting: %v", err)
	}
	if err := s.ContinueWithoutAccount(); !errors.As(err, &we) || we.Code != string(CodeNotReady) {
		t.Fatalf("ContinueWithoutAccount before connecting: %v", err)
	}
	a := &accountSetting{}
	ConnectDesktop(s, DesktopParts{Account: a})
	if without, err := s.WithoutAccount(); err != nil || without {
		t.Fatalf("WithoutAccount = %v, %v; want false", without, err)
	}
	if err := s.ContinueWithoutAccount(); err != nil {
		t.Fatal(err)
	}
	if without, err := s.WithoutAccount(); err != nil || !without {
		t.Fatalf("after continuing: %v, %v", without, err)
	}
	b := &accountSetting{fail: &transport.Error{Code: "app.account_save_failed", Params: map[string]string{"detail": "read-only"}}}
	ConnectDesktop(s, DesktopParts{Account: b})
	if err := s.ContinueWithoutAccount(); !errors.As(err, &we) || we.Code != "app.account_save_failed" || we.Params["detail"] != "read-only" {
		t.Fatalf("ContinueWithoutAccount failure = %v", err)
	}
	if without, _ := s.WithoutAccount(); !without {
		t.Fatal("a failed save did not count for this run")
	}
}

// DV17、D149：接上之前不猜（not_ready），之後回這個構建有沒有遠程。
func TestDesktopRemoteAvailable(t *testing.T) {
	for _, want := range []bool{true, false} {
		s := &Desktop{}
		var we *transport.Error
		if _, err := s.RemoteAvailable(); !errors.As(err, &we) || we.Code != string(CodeNotReady) {
			t.Fatalf("before connecting: %v", err)
		}
		ConnectDesktop(s, DesktopParts{Remote: want})
		if got, err := s.RemoteAvailable(); err != nil || got != want {
			t.Fatalf("connected with Remote %v: got %v, %v", want, got, err)
		}
	}
}
