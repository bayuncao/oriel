package app

// release_notes.go：更新說明的 mygo 接線（m2.md 第 27 節，D142；README 規則 8 允許 app 匯入 mygo）。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/updater"
)

// releaseRepos 是各渠道放 Release 的 GitHub 倉庫，按應用名稱（mygo.config.ts 的 name）分；
// 本機構建與發布構建同名，但沒有更新源，不拉取（RN08）。須與 mygo.config.ts 裡有更新源的渠道相同。
var releaseRepos = map[string]string{
	"OrielHub":         "Kynos-AI/OrielHub-Releases",
	"OrielHub Staging": "Kynos-AI/OrielHub-Releases-Staging",
}

// releaseNotesFile 是每個 Release 上的更新說明。
const releaseNotesFile = "release-notes.json"

// NewReleaseNotes 是更新後第一次啟動顯示的更新說明：版本與更新源來自 mygo，記錄檔在
// dataDir。只建構不做 I/O；Start 才判斷這個構建有沒有更新源。
func NewReleaseNotes(dataDir string) *updater.Notes {
	repo := releaseRepos[mygo.App.Name()]
	return updater.NewNotes(updater.NotesOptions{
		Current: mygo.App.Version(),
		Record:  filepath.Join(dataDir, "release-notes-seen.json"),
		Enabled: func() bool { return repo != "" && mygo.Updater.Enabled() },
		Fetch: func(ctx context.Context, version string) ([]byte, error) {
			// 與檢查更新同一個 client（mygo 用 http.DefaultClient），代理設定一樣（RN12）。
			return fetchReleaseNotes(ctx, http.DefaultClient, releaseNotesURL(repo, version))
		},
	})
}

// releaseNotesURL 是 repo 裡 v<version> 那個 Release 上的 release-notes.json。
func releaseNotesURL(repo, version string) string {
	return "https://github.com/" + repo + "/releases/download/v" + url.PathEscape(version) + "/" + releaseNotesFile
}

// fetchReleaseNotes 取 link 的內容；不是 200 時回錯誤。
func fetchReleaseNotes(ctx context.Context, client *http.Client, link string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", link, resp.Status)
	}
	// 多讀一個位元組，讓 updater 看得出超過上限。
	return io.ReadAll(io.LimitReader(resp.Body, updater.MaxReleaseNotes+1))
}
