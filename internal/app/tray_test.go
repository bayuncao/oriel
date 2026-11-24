package app

import (
	"context"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/locales"
)

// TR01–TR06：WatchTasks 推來的列折成兩個數。
func TestTaskCounts(t *testing.T) {
	at := func(s int) time.Time { return time.Unix(int64(s), 0) }
	for _, tc := range []struct {
		name string
		rows []hub.TaskView
		want TaskCounts
	}{
		{name: "TR01 沒有 Task", want: TaskCounts{}},
		{name: "TR02 有 Session 在跑的 Task 各算一個",
			rows: []hub.TaskView{{ID: "a", Running: 2}, {ID: "b", Running: 1}, {ID: "c"}},
			want: TaskCounts{Running: 2, Working: 2}},
		{name: "TR03 待答提問跨 Task 相加",
			rows: []hub.TaskView{{ID: "a", PendingAsks: 2}, {ID: "b", PendingAsks: 1}},
			want: TaskCounts{Attention: 3}},
		{name: "TR04 掛起的操作與待答提問相加；待確認還不在“需要處理”裡，不算（m2.md D09）",
			rows: []hub.TaskView{{ID: "a", PendingAsks: 1, Failed: 2, Confirmations: 2}, {ID: "b", Failed: 1}},
			want: TaskCounts{Attention: 4, Waiting: 1}}, // SL11 有待確認的 a 算在等
		{name: "TR05 已歸檔的 Task 不算",
			rows: []hub.TaskView{{ID: "a", Archived: true, Running: 1, PendingAsks: 2, Failed: 1}, {ID: "b", Running: 1}},
			want: TaskCounts{Running: 1, Working: 1}},
		{name: "TR06 同一 ID 取 Version 較大的列，不看 UpdatedAt",
			rows: []hub.TaskView{
				{ID: "a", Running: 1, PendingAsks: 1, Version: 5, UpdatedAt: at(2)},
				{ID: "a", Version: 4, UpdatedAt: at(1)}, // 重複推送的舊列
				{ID: "b", Running: 1, Version: 2, UpdatedAt: at(1)},
				{ID: "b", PendingAsks: 4, Version: 3, UpdatedAt: at(3)}, // 新列取代
				{ID: "c", PendingAsks: 1, Version: 7, UpdatedAt: at(1)},
				{ID: "c", Running: 1, Version: 6, UpdatedAt: at(1)}, // 同一刻的舊列
				{ID: "d", Running: 1, Version: 2, UpdatedAt: at(2)},
				{ID: "d", Version: 3, UpdatedAt: at(1)}, // 時鐘倒退後的新列
			},
			want: TaskCounts{Running: 1, Attention: 6, Waiting: 1}}, // 在跑的 a 停在待答審批上，不算在幹活，算在等
		{name: "KA04 在跑但停在待答審批上的不算在幹活",
			rows: []hub.TaskView{{ID: "a", Running: 1, PendingAsks: 1}, {ID: "b", Running: 1}},
			want: TaskCounts{Running: 2, Attention: 1, Working: 1, Waiting: 1}},
		{name: "SL01 在跑又有待答審批的 Task 直接算在等，不由 Running 減 Working 得出",
			rows: []hub.TaskView{{ID: "a", Running: 1, PendingAsks: 1}, {ID: "b", Running: 1}, {ID: "c", Confirmations: 1},
				{ID: "d", PendingAsks: 1}}, // d 沒有在跑：不算在等
			want: TaskCounts{Running: 2, Attention: 2, Working: 1, Waiting: 2}}, // Running − Working 會是 1
		{name: "SL11 只剩待確認也算在等，沒有 Session 在跑也算",
			rows: []hub.TaskView{{ID: "a", Confirmations: 1}, {ID: "b", Archived: true, Confirmations: 1}},
			want: TaskCounts{Waiting: 1}},
		{name: "SL09 歸檔的、停在待答審批上的不算在等（Running 與 Working 都不算）",
			rows: []hub.TaskView{{ID: "a", Archived: true, Running: 1, PendingAsks: 1}},
			want: TaskCounts{}},
		{name: "KA08 歸檔的不算在幹活",
			rows: []hub.TaskView{{ID: "a", Archived: true, Running: 1}},
			want: TaskCounts{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f taskFold
			for _, r := range tc.rows {
				f.apply(r)
			}
			if got := f.counts(); got != tc.want {
				t.Fatalf("counts = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TR07：圖標旁的文字固定「運行中 · 需處理」，兩個都是 0 時不顯示。
func TestTrayTitle(t *testing.T) {
	for _, tc := range []struct {
		c    TaskCounts
		want string
	}{
		{TaskCounts{}, ""},
		{TaskCounts{Running: 2, Attention: 1}, "2 · 1"},
		{TaskCounts{Running: 2}, "2 · 0"},
		{TaskCounts{Attention: 3}, "0 · 3"},
		{TaskCounts{Running: 1, Working: 1}, "1 · 0"},
	} {
		if got := tc.c.Title(); got != tc.want {
			t.Errorf("%+v.Title() = %q, want %q", tc.c, got, tc.want)
		}
	}
}

// TR08：提示文字在每種語言都說明兩個數。
func TestTrayToolTip(t *testing.T) {
	if got := (TaskCounts{}).ToolTip(); got != "OrielHub" {
		t.Errorf("zero ToolTip = %q", got)
	}
	for _, loc := range []string{"en", "zh-Hans"} {
		for _, c := range []TaskCounts{{Running: 1, Attention: 1}, {Running: 2, Attention: 5}} {
			got := locales.In(loc).TrayTooltip(c.Running, c.Attention)
			if got == "" || got == "tray_tooltip" {
				t.Errorf("%s %+v: tooltip %q", loc, c, got)
			}
		}
	}
}

// TR09：ctx 結束或 hub 關閉都回 nil。
func TestWatchTaskCounts(t *testing.T) {
	a, _ := newTestApp(t, proc.Env{}, t.TempDir())
	ctx, cancel := context.WithCancel(ctxb2())
	done := make(chan error, 1)
	go func() { done <- WatchTaskCounts(ctx, a.Hub(), func(TaskCounts) {}) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("after cancel: %v", err)
	}
	a.Close()
	if err := WatchTaskCounts(ctxb2(), a.Hub(), func(TaskCounts) {}); err != nil {
		t.Fatalf("after close: %v", err)
	}
}

// 刪掉的 Task（DeleteTask 推一列 Removed，只有 ID、Version 為 0）從折好的列裡拿掉，不一直留著；
// 刪除前一定已歸檔，所以數字不變。
func TestTaskFoldDropsRemoved(t *testing.T) {
	var f taskFold
	f.apply(hub.TaskView{ID: "a", Archived: true, Version: 9})
	f.apply(hub.TaskView{ID: "b", Running: 1, Version: 3})
	f.apply(hub.TaskView{ID: "a", Removed: true})
	if _, ok := f.tasks["a"]; ok || len(f.tasks) != 1 {
		t.Fatalf("rows %+v", f.tasks)
	}
	if c := f.counts(); c != (TaskCounts{Running: 1, Working: 1}) {
		t.Fatalf("counts %+v", c)
	}
}
