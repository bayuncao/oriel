package app

// window.go：主窗口第一次打開時的大小，按屏幕工作區的比例算。

// 主窗口的大小（DIP）：工作區的 3/4，不大於 1440×960，不小於
// MainWindowMin*（也是窗口能縮到的最小尺寸），且不超出工作區。
const (
	MainWindowMinWidth  = 480
	MainWindowMinHeight = 360
	mainWindowMaxWidth  = 1440
	mainWindowMaxHeight = 960
	// 拿不到顯示器時的大小。
	mainWindowFallbackWidth  = 960
	mainWindowFallbackHeight = 640
)

// MainWindowSize 是主窗口第一次打開時的大小，workWidth、workHeight 是主顯示器
// 的工作區（扣掉菜單欄與 Dock）。寬、高各自計算。窗口有記住的狀態時
// mygo 用記住的大小，這裡的值不起作用。
func MainWindowSize(workWidth, workHeight int) (width, height int) {
	if workWidth <= 0 || workHeight <= 0 {
		return mainWindowFallbackWidth, mainWindowFallbackHeight
	}
	return mainWindowSide(workWidth, MainWindowMinWidth, mainWindowMaxWidth),
		mainWindowSide(workHeight, MainWindowMinHeight, mainWindowMaxHeight)
}

// mainWindowSide 是一個方向的長度：work 的 3/4，夾在 [lo, hi]，再不超過 work。
func mainWindowSide(work, lo, hi int) int {
	return min(max(min(work*3/4, hi), lo), work)
}
