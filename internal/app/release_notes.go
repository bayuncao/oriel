package app

// release_notes.go：更新說明的 mygo 接線（m2.md 第 27 節，D142；README 規則 8 允許 app 匯入 mygo）。

import (
	"context"
	"path/filepath"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/updater"
)

// NewReleaseNotes 是更新後第一次啟動顯示的更新說明：版本來自 mygo，從這個
// 渠道的更新來源拉取（先 GitHub、再備用源，US08），記錄檔在 dataDir。只建
// 構不做 I/O；Start 才判斷這個構建有沒有更新源。
func NewReleaseNotes(dataDir string) *updater.Notes {
	feed := channelFeed()
	return updater.NewNotes(updater.NotesOptions{
		Current: mygo.App.Version(),
		Record:  filepath.Join(dataDir, "release-notes-seen.json"),
		Enabled: func() bool { return feed != nil && mygo.Updater.Enabled() },
		Fetch: func(ctx context.Context, version string) ([]byte, error) {
			return feed.ReleaseNotes(ctx, version)
		},
	})
}
