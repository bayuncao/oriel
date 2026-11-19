package main

import (
	"runtime"
	"testing"

	"github.com/egoist/mygo"
)

// TB01、TB13：macOS 隱藏標題欄、交通燈在那一行裡垂直居中；其他平台保留系統標題欄。
func TestMainTitleBar(t *testing.T) {
	if runtime.GOOS == "darwin" {
		if mainTitleBar != mygo.TitleBarHiddenInset {
			t.Errorf("mainTitleBar = %q, want %q", mainTitleBar, mygo.TitleBarHiddenInset)
		}
		if mainTrafficLights == nil {
			t.Fatal("mainTrafficLights = nil, want a position")
		}
		if top, bottom := mainTrafficLights.Y, titleBarRow-mainTrafficLights.Y-trafficLightSize; top != bottom {
			t.Errorf("traffic lights at Y=%d leave %d above and %d below in a %d row", mainTrafficLights.Y, top, bottom, titleBarRow)
		}
		return
	}
	if mainTitleBar != mygo.TitleBarDefault || mainTrafficLights != nil {
		t.Errorf("mainTitleBar = %q, mainTrafficLights = %v; want the system title bar", mainTitleBar, mainTrafficLights)
	}
}
