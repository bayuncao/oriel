package main

import (
	"context"
	_ "embed"
	"log"
	"log/slog"
	"os"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/app"
	"github.com/bayuncao/oriel/internal/transport/mygoipc"
	"github.com/bayuncao/oriel/internal/updater"

	// 翻譯目錄：使其語言成為 app 的語言（docs/architecture/README.md 規則 9）。
	_ "github.com/bayuncao/oriel/locales"
)

var hubSvc = &mygoipc.Hub{}
var agentsSvc = &mygoipc.Agents{}
var desktopSvc = &mygoipc.Desktop{}
var usageSvc = &mygoipc.Usage{}
var providersSvc = &mygoipc.Providers{}
var keysSvc = &mygoipc.Keys{}
var integrationsSvc = &mygoipc.Integrations{}
var remoteSvc = &mygoipc.Remote{}

// win 是主窗口，只在主線程上讀寫：以下回調都在主線程上執行（mygo 的 handler
// 約定），頁面的 Desktop.ShowMain 也把 showMain 交給主線程，所以這裡不需要同步。
var win *mygo.Window

// showMain 有活窗口就恢復並聚焦，沒有就開一個；開窗時 Dock 圖標回來。只在主線程上呼叫。
func showMain() {
	if win != nil {
		win.Restore()
		win.Focus()
		return
	}
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyRegular)
	// 第一次打開按主顯示器工作區的比例定大小；記住的狀態（StateKey）優先。
	work := mygo.Screen.PrimaryDisplay().WorkArea
	width, height := app.MainWindowSize(work.Width, work.Height)
	w := mygo.NewWindow(mygo.WindowOptions{
		Title:                "OrielHub",
		Width:                width,
		Height:               height,
		MinWidth:             app.MainWindowMinWidth,
		MinHeight:            app.MainWindowMinHeight,
		BackgroundColor:      "light-dark(#f6f7f9, #0f1115)",
		TitleBarStyle:        mainTitleBar,
		TrafficLightPosition: mainTrafficLights,
		StateKey:             "main",
		URL:                  "/",
	})
	w.OnClosed(func() {
		win = nil
		// 關窗後只留菜單欄：沒有菜單欄圖標時保留 Dock 圖標，否則回不到窗口。
		if tray != nil {
			mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
		}
	})
	win = w
}

// trayIcon 是 OrielHub 的窗形標誌（由 brand/mark.svg 導出，bun run icons），32×32 黑色透明底，由 macOS 按菜單欄染色。
//
//go:embed resources/tray.png
var trayIcon []byte

// tray 是菜單欄圖標，panel 是它打開著的面板（沒有則 nil）；只在主線程上讀寫。
var (
	tray  *mygo.Tray
	panel *mygo.Window
)

// togglePanel 在圖標下打開面板（/tray 頁），已打開就關掉。
func togglePanel() {
	if panel != nil {
		panel.Close()
		return
	}
	panel = mygo.NewFlyout(mygo.FlyoutOptions{
		Tray:      tray,
		Placement: mygo.PlacementBottom,
		Width:     320,
		Height:    420,
		Focusable: true,
		Popover:   true,
		URL:       "/tray",
	})
	panel.OnClosed(func() { panel = nil })
}

// trayCounts 是圖標旁最近一次的數字；只在主線程上讀寫。
var trayCounts app.TaskCounts

// showTrayCounts 依目前的語言畫出圖標旁的數字與提示文字；只在主線程上呼叫。
func showTrayCounts() {
	tray.SetTitle(trayCounts.Title())
	tray.SetToolTip(trayCounts.ToolTip())
}

// startTray 放上菜單欄圖標（docs/acceptance/m1.md 菜單欄一節）；數字由 watchCounts 跟著 hub 的 Task 列表更新。
func startTray() {
	t, err := mygo.NewTray(mygo.TrayOptions{
		ID:             "main",
		Icon:           trayIcon,
		IconIsTemplate: true,
		ToolTip:        trayCounts.ToolTip(),
	})
	if err != nil {
		slog.Warn("tray unavailable", "err", err)
		return
	}
	tray = t
	t.OnClick(togglePanel)
	// 換語言時提示文字立刻跟著換，不等數字變化（I03）。
	mygo.App.OnLocaleChanged(func(string) { showTrayCounts() })
}

// watchCounts 跟著 hub 的 Task 列表更新菜單欄的數字與防止睡眠的聲明（D80）；沒有菜單欄圖標時也要防止睡眠，
// 所以不放在 startTray 裡。
func watchCounts(a *app.App, awake *app.KeepAwake) {
	// 在這個 goroutine 上：KeepAwake 等主線程，不能在主線程上呼叫。
	err := app.FollowTaskCounts(context.Background(), a.Hub(), awake, func(c app.TaskCounts) {
		// 交給主線程（app 已就緒，WhenReady 即排進主線程）：與換語言的重畫
		// 依序執行，提示文字總是最新的數字與當下的語言。
		mygo.App.WhenReady(func() {
			trayCounts = c
			if tray != nil {
				showTrayCounts()
			}
		})
	})
	if err != nil {
		slog.Warn("task counts stopped", "err", err)
	}
}

// warnLanguage 記下語言選擇讀不了的原因（這時跟隨系統，D73）；err 為 nil 時不記。
func warnLanguage(err error) {
	if err != nil {
		slog.Warn("language choice unreadable; following the system", "err", err)
	}
}

func main() {
	if !mygo.App.RequestSingleInstanceLock() {
		return // 第二個實例：交給第一個實例喚起視窗後退出
	}
	mygo.BindAs("Hub", hubSvc)
	mygo.BindAs("Agents", agentsSvc)
	mygo.BindAs("Desktop", desktopSvc)
	mygo.BindAs("Usage", usageSvc)
	mygo.BindAs("Providers", providersSvc)
	mygo.BindAs("Keys", keysSvc)
	mygo.BindAs("Integrations", integrationsSvc)
	// 每個渠道都綁 Remote，src/mygo.ts 才不隨渠道而變，前端在正式版照樣編譯（DV19）。正式版先沒有
	// 遠程（D149）：停用它，頁面呼叫它立即失敗，不會當作“還在啟動”一直重試（DV18）。
	mygo.BindAs("Remote", remoteSvc)
	if !app.RemoteAvailable() {
		mygoipc.DisableRemote(remoteSvc)
	}
	// 自動更新（ADR-27、D76）：菜單欄面板的更新按鈕經 Desktop 讀它。
	updates := app.NewUpdater()

	var a *app.App
	// 更新後第一次啟動的更新說明（m2.md 第 27 節）：知道資料目錄才建。
	var notes *updater.Notes
	// 事件迴圈停下後關掉更新與後端：退出已不能取消，而且 App.Relaunch 在
	// OnQuit 之後才啟動新實例，新實例打開資料庫時舊的已經關好（D58）。
	// OnBeforeQuit 會擋 UI，退出也仍可能被取消，所以不用。
	mygo.App.OnQuit(func() {
		updates.Close()
		if notes != nil {
			notes.Close()
		}
		if a != nil {
			a.Close()
		}
	})
	mygo.App.OnSecondInstance(func(_ []string, _ string) {
		showMain()
	})
	mygo.App.OnWindowAllClosed(func() {}) // macOS 慣例：不退出
	mygo.App.OnActivate(func(hasVisible bool) {
		if !hasVisible {
			showMain()
		}
	})
	// 通知點擊在 Run 之前登記：上一輪留下的通知也可能喚起 app。
	mygo.App.OnNotificationClick(func(id string) {
		if tid, ok := app.TaskOfNotification(id); ok {
			showMain()
			// 頁面未載入時事件會在 ready 後送達。
			mygoipc.OpenTask.Broadcast(mygoipc.OpenTaskEvent{Task: tid})
		}
	})

	// 碰檔案與進程的事都從這裡開始：mygo generate 也執行 main，到 Run 為止
	// （docs/architecture/README.md 的 mygo 約束）。
	mygo.App.WhenReady(func() {
		// 判斷能否自我更新要碰檔案系統，不在主線程上做。
		go updates.Start()
		dataDir, err := mygo.App.Path(mygo.PathUserData)
		if err != nil {
			mygo.Dialog.Error("OrielHub", app.RenderError(err))
			mygo.App.Quit()
			return
		}
		// 用戶選單選的介面語言（D73）記在資料目錄裡。讀不了時跟隨系統；這時日誌
		// 還沒打開，原因留到打開之後再記，免得只寫到打包的 app 看不到的 stderr。
		lang, langErr := app.OpenLanguage(dataDir, mygo.App.Languages())
		// 開窗之前把記住的語言交還 MyGo，兩份記錄不分岔；
		// MYGO_LOCALE（mygo dev -lang）是這次執行選的語言，以它為準。
		if os.Getenv("MYGO_LOCALE") == "" {
			mygo.App.SetLocale(lang.Choice())
		}
		// 外觀（D158）也記在資料目錄裡；開窗之前交給 MyGo，一打開就是記住的那種（PF04）。
		// 讀不了時跟隨系統，原因同樣等日誌打開再記。
		theme, themeErr := app.OpenTheme(dataDir)
		theme.Apply()
		notes = app.NewReleaseNotes(dataDir)
		// Agent 運行時防止電腦睡眠的開關（D80）也記在資料目錄裡；讀不了時是開的，原因同樣等日誌打開再記。
		awake, awakeErr := app.OpenKeepAwake(dataDir, app.SystemKeepAwake)
		logDir, err := mygo.App.Path(mygo.PathLogs)
		if err != nil {
			warnLanguage(langErr) // 沒有日誌檔：寫到 stderr，mygo dev 看得到
			mygo.Dialog.Error("OrielHub", app.RenderError(err))
			mygo.App.Quit()
			return
		}
		repo, mirror := app.UpdateSource()
		// 在後端與第一個視窗之前接上：頁面不必等後端就能用 Desktop。
		mygoipc.ConnectDesktop(desktopSvc, mygoipc.DesktopParts{
			Show: showMain, Lang: lang, Updates: updates, Notes: notes, KeepAwake: awake, Theme: theme, Remote: app.RemoteAvailable(),
			UpdateSource: mygoipc.UpdateSource{Repo: repo, Mirror: mirror},
			Folders:      map[mygoipc.PrivacyFolder]string{mygoipc.FolderData: dataDir, mygoipc.FolderLogs: logDir},
			Worktrees:    app.WorktreeRoot(dataDir)})
		// 換語言後聲明的原因跟著換；OnLocaleChanged 在主線程上，KeepAwake 等主線程，所以另開 goroutine。
		mygo.App.OnLocaleChanged(func(string) { go awake.Relabel() })
		// 日誌檔打不開時照舊寫到 stderr（mygo dev 看得到）並記下原因；logger 不留 nil，
		// 後面只在有 logger 時才記的警告（如通知）才不會跟著消失。
		logger := slog.Default()
		if f, err := app.OpenLog(logDir); err != nil {
			slog.Warn("log file unavailable; logging to stderr", "dir", logDir, "err", err)
		} else {
			logger = slog.New(slog.NewTextHandler(f, nil))
			slog.SetDefault(logger)
		}
		warnLanguage(langErr)
		if awakeErr != nil {
			slog.Warn("keep-awake choice unreadable; keeping awake while agents run", "err", awakeErr)
		}
		if themeErr != nil {
			slog.Warn("theme choice unreadable; following the system", "err", themeErr)
		}
		// 日誌打開之後才開始：拉取失敗要記進日誌檔。
		notes.Start()
		a, err = app.New(context.Background(), app.Options{
			DataDir:  dataDir,
			Secret:   app.Secret,
			Keychain: app.Keychain{},
			Notifier: app.Notifier(logger),
			Logger:   logger,
			OnResume: mygo.Power.OnResume,
			Version:  mygo.App.Version(),
			Remote:   app.RemoteAvailable(),
		})
		if err != nil {
			mygo.Dialog.Error("OrielHub", app.RenderError(err))
			mygo.App.Quit()
			return
		}
		mygoipc.ConnectHub(hubSvc, a.Hub(), logger)
		mygoipc.ConnectAgents(agentsSvc, a.Agents(), logger)
		mygoipc.ConnectUsage(usageSvc, a.Usage(), logger)
		mygoipc.ConnectProviders(providersSvc, a.Providers(), logger)
		mygoipc.ConnectKeys(keysSvc, a.Keys(), logger)
		mygoipc.ConnectIntegrations(integrationsSvc, a.Integrations(), logger)
		if rc := a.Remote(); rc != nil {
			mygoipc.ConnectRemote(remoteSvc, rc, logger)
		}
		go mygoipc.BroadcastChanges(a.Changes(), a.Agents())
		startTray()
		go watchCounts(a, awake)
		showMain()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
