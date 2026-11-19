package app

import "testing"

// MW01–MW06：主窗口第一次打開時的大小（S7，有記住的狀態時用記住的，是 mygo 的行為，不在此測）。
func TestMainWindowSize(t *testing.T) {
	for _, tc := range []struct {
		name         string
		workW, workH int
		wantW, wantH int
	}{
		{"MW01 13 寸 MacBook 取工作區的 3/4", 1440, 875, 1080, 656},
		{"MW02 大屏超過上限取上限", 2560, 1415, 1440, 960},
		{"MW03 寬到上限、高按比例", 1920, 1055, 1440, 791},
		{"MW04 3/4 低於最小尺寸時提到最小尺寸", 560, 400, 480, 360},
		{"MW05 工作區比最小尺寸還小時取整個工作區", 400, 300, 400, 300},
		{"MW06 拿不到顯示器時用 960×640", 0, 0, 960, 640},
		{"MW06 只有一邊是 0 也算拿不到", 1440, 0, 960, 640},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, h := MainWindowSize(tc.workW, tc.workH)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("MainWindowSize(%d, %d) = %d×%d, want %d×%d", tc.workW, tc.workH, w, h, tc.wantW, tc.wantH)
			}
		})
	}
}
