//go:build !darwin

package main

import "github.com/egoist/mygo"

// 其他平台保留系統標題欄（D140，m2.md TB13）：GTK 的按鈕放哪、有幾個由桌面決定。
var (
	mainTitleBar      = mygo.TitleBarDefault
	mainTrafficLights *mygo.Point
)
