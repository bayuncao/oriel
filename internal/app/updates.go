package app

// updates.go：自動更新的 mygo 接線（ADR-27、D76；README 規則 8 允許 app 匯入 mygo）。

import (
	"context"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/updater"
)

// NewUpdater 是經 mygo.Updater 自我更新的 updater：檢查與安裝走
// mygo.Updater，重啟走 App.Relaunch，醒來的通知來自 Power.OnResume。
// 只建構不做 I/O，可在 App.Run 之前呼叫；Start 才判斷這個構建能否自我更新。
func NewUpdater() *updater.Updater {
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
