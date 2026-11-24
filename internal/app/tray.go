package app

// tray.go：Task 列表折成的幾個數：菜單欄圖標旁的數字（docs/acceptance/m1.md 菜單欄一節 M02、M03），
// 與 Agent 運行時防止睡眠要的“在幹活”（D80，m2.md 第 30 節）。

import (
	"context"
	"errors"
	"strconv"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/locales"
)

// TaskCounts 是從 Task 列表算出的數，只算未歸檔的 Task。
type TaskCounts struct {
	Running   int // 有 Session 在跑的 Task 數（菜單欄）
	Attention int // “需要處理”的條數：待答審批與掛起的操作之和（TaskView.PendingAsks + Failed，hub.Attention 的口徑，D68；菜單欄）
	// Working 是 agent 在幹活的 Task 數：有 Session 在跑、沒有待答的審批（KA01、KA04）。M2 一個 Task 最多
	// 一個 agent（D95），所以這等於有 agent 不是在等人；一個 Task 有幾個 agent 時（M4）要改成按 Session 算。
	Working int
	// Waiting 是有人可能正要從手機處理的 Task 數（D146、SL01、SL11）：有 Session 在跑而停在待答的審批上，
	// 或有待確認。有遠程時這些也不讓電腦空閒睡眠。直接按 Task 數，不從 Running 與 Working 相減：
	// Working 改按 Session 算（M4）時兩者口徑不同。
	Waiting int
}

// Title 是圖標旁的文字「運行中 · 需處理」；兩個都是 0 時為空。
func (c TaskCounts) Title() string {
	if c.Running == 0 && c.Attention == 0 {
		return ""
	}
	return strconv.Itoa(c.Running) + " · " + strconv.Itoa(c.Attention)
}

// ToolTip 說明兩個數各是什麼；兩個都是 0 時只有 app 名。
func (c TaskCounts) ToolTip() string {
	if c.Running == 0 && c.Attention == 0 {
		return "OrielHub"
	}
	return locales.TrayTooltip(c.Running, c.Attention)
}

// taskFold 把 WatchTasks 推來的列折成 TaskCounts。
type taskFold struct {
	tasks map[task.TaskID]hub.TaskView
}

// apply 收一列：同一 ID 以 Version 較大者為準（WatchTasks 是 at-least-once；UpdatedAt 可能同刻或倒退）；
// Removed 的列（只有 ID，DeleteTask）把這個 Task 拿掉。
func (f *taskFold) apply(v hub.TaskView) {
	if f.tasks == nil {
		f.tasks = map[task.TaskID]hub.TaskView{}
	}
	if v.Removed {
		delete(f.tasks, v.ID)
		return
	}
	if cur, ok := f.tasks[v.ID]; ok && v.Version < cur.Version {
		return
	}
	f.tasks[v.ID] = v
}

// counts 用目前的列算出兩個數。
func (f *taskFold) counts() TaskCounts {
	var c TaskCounts
	for _, v := range f.tasks {
		if v.Archived {
			continue
		}
		if v.Running > 0 {
			c.Running++
			if v.PendingAsks == 0 {
				c.Working++
			}
		}
		if v.Running > 0 && v.PendingAsks > 0 || v.Confirmations > 0 {
			c.Waiting++
		}
		c.Attention += v.PendingAsks + v.Failed // 待確認（M5）與評審推薦（M4）進列表時一起加
	}
	return c
}

// WatchTaskCounts 跟著 hub 的 Task 列表在數字變化時呼叫 sink（初值都是 0，不推）。
// 觀察者落後被踢出時重新訂閱；ctx 結束或 hub 關閉（應用退出）回 nil。
func WatchTaskCounts(ctx context.Context, h *hub.Hub, sink func(TaskCounts)) error {
	var last TaskCounts
	// 重新訂閱時沿用已折好的列：新的訂閱從頭再推一遍全集，推到一半時的數不能當真（數字會閃、防止睡眠的
	// 聲明會先放再拿）。同一 ID 取 Version 較大的，所以舊列只會被同一 Task 的新列取代；刪掉的 Task 推一列 Removed，從折好的列裡拿掉。
	var f taskFold
	for {
		err := h.WatchTasks(ctx, hub.LocalOwner, func(v hub.TaskView) error {
			f.apply(v)
			if c := f.counts(); c != last {
				last = c
				sink(c)
			}
			return nil
		})
		if ctx.Err() != nil || errors.Is(err, hub.ErrClosed) {
			return nil
		}
		var he *hub.Error
		if !errors.As(err, &he) || he.Code != hub.CodeLagging {
			return err
		}
	}
}

// FollowTaskCounts 同 WatchTaskCounts，另外把“在幹活”與“有 Task 在等人處理”交給 awake（D80、D146）；
// 不論為什麼結束，結束時都告訴它兩者都沒有，不留著聲明。不能在主線程上呼叫（KeepAwake 等主線程）。
func FollowTaskCounts(ctx context.Context, h *hub.Hub, awake *KeepAwake, sink func(TaskCounts)) error {
	defer awake.SetTasks(false, false)
	return WatchTaskCounts(ctx, h, func(c TaskCounts) {
		awake.SetTasks(c.Working > 0, c.Waiting > 0)
		sink(c)
	})
}
