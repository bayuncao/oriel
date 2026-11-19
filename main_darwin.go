//go:build darwin

package main

import "github.com/egoist/mygo"

// 主窗口隱藏系統標題欄，只留交通燈（D140，m2.md TB01）：交通燈在側欄品牌行左側，
// 在 titleBarRow 裡垂直居中，左邊與原來的標誌對齊（18）；頁面按 --mygo-titlebar-inset-left 讓位。
var (
	mainTitleBar      = mygo.TitleBarHiddenInset
	mainTrafficLights = &mygo.Point{X: 18, Y: (titleBarRow - trafficLightSize) / 2}
)
