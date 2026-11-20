package updater

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 更新源的驗收場景 US01–US08（m2.md 第 34 節）：兩個本機 HTTP 服務扮演
// GitHub 的 Release 與備用源，Feed 的 GitHub 前綴指向前者。

const manifestName = "update-darwin-arm64.json"

// site 是一個扮演 GitHub 或備用源的本機服務：files 是路徑 → 內容；fail
// 是回 500 的路徑；hang 為真時不回應，直到請求被取消。
type site struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string]string
	fail  bool
	hang  bool
	hits  []string
	// arrived 每收到一個請求送一次（不阻塞）；測試靠它等請求到了。
	arrived chan struct{}
}

func newSite(t *testing.T, files map[string]string) *site {
	t.Helper()
	s := &site{files: files, arrived: make(chan struct{}, 64)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits = append(s.hits, r.URL.Path)
		fail, hang := s.fail, s.hang
		body, ok := s.files[r.URL.Path]
		s.mu.Unlock()
		select {
		case s.arrived <- struct{}{}:
		default:
		}
		if r.Header.Get("User-Agent") != "orielhub-updater" {
			http.Error(w, "bad user agent", http.StatusBadRequest)
			return
		}
		switch {
		case hang:
			<-r.Context().Done()
		case fail:
			http.Error(w, "boom", http.StatusInternalServerError)
		case !ok:
			http.NotFound(w, r)
		default:
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *site) set(fail, hang bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail, s.hang = fail, hang
}

func (s *site) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hits)
}

// newTestFeed 是指向本機 GitHub 與備用源的 Feed；mirror 為 nil 時沒有備用源。
func newTestFeed(gh, mirror *site, wait time.Duration) *Feed {
	o := FeedOptions{Repo: "bayuncao/oriel", Manifest: manifestName, Wait: wait}
	if mirror != nil {
		o.Mirror = mirror.srv.URL + "/oriel/"
	}
	f := NewFeed(o)
	f.github = gh.srv.URL + "/bayuncao/oriel/releases/"
	return f
}

// reader 讀完 Manifest、Open 的結果：reader(t)(f.Manifest(ctx, false))。
func reader(t *testing.T) func(io.ReadCloser, error) string {
	return func(body io.ReadCloser, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		defer body.Close()
		b, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

var (
	ghFiles = map[string]string{
		"/bayuncao/oriel/releases/latest/download/" + manifestName:     "github manifest",
		"/bayuncao/oriel/releases/download/v1.2.0/app-1.2.0.tar.gz":    "github archive",
		"/bayuncao/oriel/releases/download/v1.2.0/" + ReleaseNotesFile: `{"version":"1.2.0"}`,
	}
	mirrorFiles = map[string]string{
		"/oriel/latest/" + manifestName:     "mirror manifest",
		"/oriel/v1.2.0/app-1.2.0.tar.gz":    "mirror archive",
		"/oriel/v1.2.0/" + ReleaseNotesFile: `{"version":"1.2.0","from":"mirror"}`,
	}
)

func TestFeedManifest(t *testing.T) {
	cases := []struct {
		name         string
		ghFail       bool
		ghHang       bool
		ghDown       bool // GitHub 的服務關了：連線失敗
		noMirror     bool
		mirrorFail   bool
		want         string
		wantErrParts []string // 都失敗時錯誤裡要有的字樣
	}{
		{name: "US01 GitHub answers", want: "github manifest"},
		{name: "US02 GitHub answers 500", ghFail: true, want: "mirror manifest"},
		{name: "US02 GitHub unreachable", ghDown: true, want: "mirror manifest"},
		{name: "US02 GitHub never answers", ghHang: true, want: "mirror manifest"},
		{name: "staging has no mirror", ghFail: true, noMirror: true, wantErrParts: []string{"500"}},
		{name: "US05 both fail", ghFail: true, mirrorFail: true, wantErrParts: []string{"/releases/latest/download/", "/oriel/latest/"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh, mirror := newSite(t, ghFiles), newSite(t, mirrorFiles)
			gh.set(tc.ghFail, tc.ghHang)
			mirror.set(tc.mirrorFail, false)
			if tc.ghDown {
				gh.srv.Close()
			}
			m := mirror
			if tc.noMirror {
				m = nil
			}
			f := newTestFeed(gh, m, 200*time.Millisecond)

			body, err := f.Manifest(t.Context(), false)
			if tc.wantErrParts != nil {
				if err == nil {
					body.Close()
					t.Fatal("got a manifest, want an error")
				}
				for _, part := range tc.wantErrParts {
					if !strings.Contains(err.Error(), part) {
						t.Fatalf("err = %v, want it to mention %q", err, part)
					}
				}
				return
			}
			if got := reader(t)(body, err); got != tc.want {
				t.Fatalf("manifest = %q, want %q", got, tc.want)
			}
			if tc.want == "github manifest" && mirror.hitCount() != 0 {
				t.Fatal("asked the mirror although GitHub answered")
			}
		})
	}
}

func TestFeedPrereleasesRefused(t *testing.T) {
	f := newTestFeed(newSite(t, ghFiles), nil, time.Second)
	if _, err := f.Manifest(t.Context(), true); err == nil {
		t.Fatal("prereleases: want an error")
	}
}

// US04：備用源成功過就先試它；它失敗而 GitHub 成功後，又先試 GitHub。
func TestFeedRemembersTheSourceThatWorked(t *testing.T) {
	gh, mirror := newSite(t, ghFiles), newSite(t, mirrorFiles)
	f := newTestFeed(gh, mirror, time.Second)

	gh.set(true, false)
	if got := reader(t)(f.Manifest(t.Context(), false)); got != "mirror manifest" {
		t.Fatalf("first check = %q", got)
	}
	gh.set(false, false)
	before := gh.hitCount()
	if got := reader(t)(f.Manifest(t.Context(), false)); got != "mirror manifest" || gh.hitCount() != before {
		t.Fatalf("second check = %q with %d GitHub requests, want the mirror first", got, gh.hitCount()-before)
	}
	mirror.set(true, false)
	if got := reader(t)(f.Manifest(t.Context(), false)); got != "github manifest" {
		t.Fatalf("third check = %q, want GitHub after the mirror failed", got)
	}
	mirror.set(false, false)
	before = mirror.hitCount()
	if got := reader(t)(f.Manifest(t.Context(), false)); got != "github manifest" || mirror.hitCount() != before {
		t.Fatalf("fourth check = %q with %d mirror requests, want GitHub first again", got, mirror.hitCount()-before)
	}
}

// US03：清單寫的是 GitHub Release 的網址，失敗時改取備用源的同名檔案；
// 別處的網址只試它本身。
func TestFeedOpen(t *testing.T) {
	gh, mirror := newSite(t, ghFiles), newSite(t, mirrorFiles)
	f := newTestFeed(gh, mirror, time.Second)
	archive := f.github + "download/v1.2.0/app-1.2.0.tar.gz"

	if got := reader(t)(f.Open(t.Context(), archive)); got != "github archive" {
		t.Fatalf("archive = %q", got)
	}
	gh.set(true, false)
	if got := reader(t)(f.Open(t.Context(), archive)); got != "mirror archive" {
		t.Fatalf("archive with GitHub failing = %q", got)
	}

	other := newSite(t, map[string]string{"/elsewhere.tar.gz": "other"})
	if got := reader(t)(f.Open(t.Context(), other.srv.URL+"/elsewhere.tar.gz")); got != "other" {
		t.Fatalf("foreign url = %q", got)
	}
	other.set(true, false)
	before := mirror.hitCount()
	if _, err := f.Open(t.Context(), other.srv.URL+"/elsewhere.tar.gz"); err == nil || mirror.hitCount() != before {
		t.Fatalf("foreign url failing: err %v, %d mirror requests; want only that url tried", err, mirror.hitCount()-before)
	}
}

func TestFeedRefusesPlainHTTP(t *testing.T) {
	f := newTestFeed(newSite(t, ghFiles), nil, time.Second)
	if _, err := f.Open(t.Context(), "http://example.com/app.tar.gz"); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// 呼叫者取消時不再試備用源。
func TestFeedStopsWhenTheCallerGivesUp(t *testing.T) {
	gh, mirror := newSite(t, ghFiles), newSite(t, mirrorFiles)
	gh.set(false, true)
	f := newTestFeed(gh, mirror, time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-gh.arrived // 請求到了 GitHub 再取消
		cancel()
	}()
	if _, err := f.Manifest(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if mirror.hitCount() != 0 {
		t.Fatal("asked the mirror after the caller gave up")
	}
}

// US08：更新說明先 GitHub、再備用源；超過上限只讀到上限多一個位元組，交給 Notes 拒絕。
func TestFeedReleaseNotes(t *testing.T) {
	gh, mirror := newSite(t, ghFiles), newSite(t, mirrorFiles)
	f := newTestFeed(gh, mirror, time.Second)
	if got, err := f.ReleaseNotes(t.Context(), "1.2.0"); err != nil || string(got) != `{"version":"1.2.0"}` {
		t.Fatalf("from GitHub: %q, %v", got, err)
	}
	gh.set(true, false)
	if got, err := f.ReleaseNotes(t.Context(), "1.2.0"); err != nil || !strings.Contains(string(got), "mirror") {
		t.Fatalf("from the mirror: %q, %v", got, err)
	}
	mirror.mu.Lock()
	mirror.files["/oriel/v1.3.0/"+ReleaseNotesFile] = strings.Repeat("x", 2*MaxReleaseNotes)
	mirror.mu.Unlock()
	if got, err := f.ReleaseNotes(t.Context(), "1.3.0"); err != nil || len(got) != MaxReleaseNotes+1 {
		t.Fatalf("too big: %d bytes, %v", len(got), err)
	}
}
