package app

// updates.go：自動更新的 mygo 接線（ADR-27、D76、D151；README 規則 8 允許 app 匯入 mygo）。

import (
	"context"
	"net/http"
	"runtime"
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/netproxy"

	"github.com/bayuncao/oriel/internal/updater"
)

// feeds 是各渠道取更新的地方，按應用名稱（mygo.config.ts 的 name）分（D151）。
// Repo 須與 mygo.config.ts 裡該渠道的 updates.github 相同；本機構建與發布
// 構建同名，但沒有更新源（mygo.Updater.Enabled 為否），不檢查也不拉取
// （US10、RN08）。正式版的 Mirror 是香港的備用源（US02）：阿里雲 OSS 的
// oriel 桶（香港），前面是同名域名的 CDN，由 scripts/publish-mirror.sh 上
// 傳；測試渠道沒有。
var feeds = map[string]updater.FeedOptions{
	"OrielHub":         {Repo: "bayuncao/oriel", Mirror: "https://updates.oriel.today"},
	"OrielHub Staging": {Repo: "Kynos-AI/OrielHub-Releases-Staging"},
}

// channelFeed 是這個構建的更新來源，更新與更新說明共用（記住成功的源，
// US04）；這個名稱沒有渠道時為 nil。
var channelFeed = sync.OnceValue(func() *updater.Feed {
	o, ok := feeds[mygo.App.Name()]
	if !ok {
		return nil
	}
	// 清單按執行的平台取：構建只出 darwin/arm64（mygo build 的預設平台），
	// 改出 darwin/universal 時清單名是 update-darwin-universal.json。
	o.Manifest = "update-" + runtime.GOOS + "-" + runtime.GOARCH + ".json"
	// 與 mygo 檢查更新同一個代理設定（US11）。
	o.Client = &http.Client{Transport: netproxy.Transport()}
	return updater.NewFeed(o)
})

// NewUpdater 是經 mygo.Updater 自我更新的 updater：檢查與安裝走
// mygo.Updater（更新源換成 channelFeed，D151），重啟走 App.Relaunch，醒
// 來的通知來自 Power.OnResume。只建構不做 I/O，可在 App.Run 之前呼叫；
// Start 才判斷這個構建能否自我更新。
func NewUpdater() *updater.Updater {
	if f := channelFeed(); f != nil {
		mygo.Updater.SetSource(f)
	}
	return updater.New(updater.Options{
		Enabled: mygo.Updater.Enabled,
		Check: func(ctx context.Context) (*updater.Release, error) {
			up, err := mygo.Updater.Check(ctx)
			if err != nil || up == nil {
				return nil, err
			}
			return &updater.Release{Version: up.Version, Install: up.Install}, nil
		},
		Relaunch: mygo.App.Relaunch,
		OnResume: mygo.Power.OnResume,
	})
}
