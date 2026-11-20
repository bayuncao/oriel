package updater

// feed.go：從哪裡取更新（D151，m2.md 第 34 節 US01–US11）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// feedWait 是每個源等回應的預設時間（US02）。
const feedWait = 15 * time.Second

// ReleaseNotesFile 是每個 Release 上的更新說明（D142）。
const ReleaseNotesFile = "release-notes.json"

// FeedOptions 是 Feed 的設定。
type FeedOptions struct {
	// Repo 是放 Release 的 GitHub 倉庫（owner/name），必填。
	Repo string
	// Mirror 是備用源的根網址，放著與 Release 相同的檔案：每個版本在
	// v<版本>/，最新版的清單另有一份在 latest/。空：沒有備用源。
	Mirror string
	// Manifest 是本平台更新清單的檔名，如 update-darwin-arm64.json。
	Manifest string
	// Client 發出請求；app 給 mygo 檢查更新所用的代理設定（US11）。
	Client *http.Client
	// Wait 是每個源等回應（收到回應標頭）的時間；0 → 15 秒。讀內容不限時：
	// 更新包可能很大。
	Wait time.Duration
	// Logger 記錄失敗而改試另一個源；nil → slog.Default()。
	Logger *slog.Logger
}

// Feed 是更新的來源：先試 GitHub 的 Release，連不上、等不到回應或不是 200
// 時改用備用源，並記住這次運行裡成功的那個，下次先試它（US01–US04）。只
// 記在記憶體裡，重啟後又先試 GitHub。簽名不歸它管：mygo 用構建時的公鑰驗
// 每個更新包，所以備用源不需要被信任（US07）。
//
// Manifest 與 Open 的簽名與 mygo.UpdateSource 相同，app 把它交給
// mygo.Updater.SetSource；updater 因此不必匯入 mygo。
type Feed struct {
	o           FeedOptions
	github      string // https://github.com/<repo>/releases/
	mirrorFirst atomic.Bool
}

// NewFeed 建立 Feed；不做 I/O。
func NewFeed(o FeedOptions) *Feed {
	if o.Wait == 0 {
		o.Wait = feedWait
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	o.Mirror = strings.TrimSuffix(o.Mirror, "/")
	return &Feed{o: o, github: "https://github.com/" + o.Repo + "/releases/"}
}

// Manifest 取本平台的更新清單：GitHub 最新 Release 上的，或備用源
// latest/ 裡的（US01、US02）。不提供預發布版本（app 不開 SetPrereleases）。
func (f *Feed) Manifest(ctx context.Context, prereleases bool) (io.ReadCloser, error) {
	if prereleases {
		return nil, errors.New("updater: the feed does not offer prereleases")
	}
	name := url.PathEscape(f.o.Manifest)
	return f.get(ctx, f.github+"latest/download/"+name, f.mirrored("latest/"+name))
}

// Open 取清單裡的一個檔案（更新包或差分包）。清單裡寫的是 GitHub Release
// 的網址，它在備用源有同名的一份：.../releases/download/<標籤>/<檔名> 對
// 應 <備用源>/<標籤>/<檔名>（US03）。不是這個倉庫 Release 的網址就只試它
// 本身。
func (f *Feed) Open(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	rest, ok := strings.CutPrefix(rawURL, f.github+"download/")
	if !ok || strings.Count(rest, "/") != 1 {
		return f.get(ctx, rawURL, "")
	}
	return f.get(ctx, rawURL, f.mirrored(rest))
}

// ReleaseNotes 取 version 那個 Release 上的 release-notes.json，最多讀
// MaxReleaseNotes 多一個位元組，讓 Notes 看得出超過上限（US08）。
func (f *Feed) ReleaseNotes(ctx context.Context, version string) ([]byte, error) {
	path := url.PathEscape("v"+version) + "/" + ReleaseNotesFile
	body, err := f.get(ctx, f.github+"download/"+path, f.mirrored(path))
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, MaxReleaseNotes+1))
}

// mirrored 是備用源上 path 的網址；沒有備用源時為空。
func (f *Feed) mirrored(path string) string {
	if f.o.Mirror == "" {
		return ""
	}
	return f.o.Mirror + "/" + path
}

// get 依序試 GitHub 與備用源（上次備用源成功時先試它），回第一個成功的；
// 都失敗時回兩者的錯誤（US05）。呼叫者的 ctx 結束時不再試下一個。
func (f *Feed) get(ctx context.Context, github, mirror string) (io.ReadCloser, error) {
	order := []string{github}
	if mirror != "" {
		if f.mirrorFirst.Load() {
			order = []string{mirror, github}
		} else {
			order = append(order, mirror)
		}
	}
	var errs []error
	for _, u := range order {
		body, err := f.fetch(ctx, u)
		if err == nil {
			if mirror != "" {
				f.mirrorFirst.Store(u == mirror)
			}
			return body, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, err)
		if len(errs) < len(order) {
			f.o.Logger.Warn("updater: source failed, trying the next", "url", u, "err", err)
		}
	}
	return nil, errors.Join(errs...)
}

// fetch 以 GET 取 rawURL：只接受 HTTPS（測試用的本機位址除外），Wait 內收
// 不到回應標頭算失敗，不是 200 算失敗。回傳的內容關閉時才放掉請求。
func (f *Feed) fetch(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname())) {
		return nil, fmt.Errorf("refusing to download %s: updates need HTTPS", rawURL)
	}
	rctx, cancel := context.WithCancelCause(ctx)
	timeout := fmt.Errorf("GET %s: no response within %s", rawURL, f.o.Wait)
	timer := time.AfterFunc(f.o.Wait, func() { cancel(timeout) })
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, err
	}
	// 只用 ASCII：GitHub 放 Release 檔案的儲存對非 ASCII 的 User-Agent 回 400。
	req.Header.Set("User-Agent", "orielhub-updater")
	resp, err := f.o.Client.Do(req)
	if !timer.Stop() {
		// 時間到了：即使回應剛好到，請求也已取消，內容讀不了。
		if err == nil {
			resp.Body.Close()
		}
		cancel(nil)
		return nil, timeout
	}
	if err != nil {
		cancel(nil)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel(nil)
		return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	return &requestBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

// requestBody 關閉時一併放掉請求的 ctx。
type requestBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (b *requestBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel(nil)
	return err
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
