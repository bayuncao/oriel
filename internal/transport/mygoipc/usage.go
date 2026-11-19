package mygoipc

import (
	"context"
	"log/slog"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/usage"
)

// Usage 是本機用量的 IPC 服務（BindAs "Usage"，docs/architecture/usage.md）。
type Usage struct{ late[*usage.Tracker] }

// ConnectUsage 給 Usage 服務接上後端（D61：包級函式，不是頁面可呼叫的方法）。
func ConnectUsage(s *Usage, t *usage.Tracker, log *slog.Logger) { s.connect(t, log) }

// Summary 回傳用量頁要的全部內容：按現在的本機時區彙總存下的用量、各額度桶最新
// 的快照與每個內建 agent 的記錄來源。
func (s *Usage) Summary(ctx context.Context) (usage.Summary, error) {
	t, log, e := s.get()
	if e != nil {
		return usage.Summary{}, e
	}
	v, err := t.Summary(ctx)
	if err != nil {
		return usage.Summary{}, wire(log, "Summary", err)
	}
	return v, nil
}

// Refresh 讀一輪各 agent 的記錄：與進行中的讀取合併，讀完才返回。頁面離開只結束
// 等待，讀取照常進行。
func (s *Usage) Refresh(ctx context.Context) (usage.ReadState, error) {
	t, log, e := s.get()
	if e != nil {
		return usage.ReadState{}, e
	}
	v, err := t.Refresh(ctx)
	if err != nil {
		return usage.ReadState{}, wire(log, "Refresh", err)
	}
	return v, nil
}

// Watch 先推目前的讀取狀態，之後每次變化推一次；頁面離開或應用退出時結束。
func (s *Usage) Watch(ctx context.Context, states *mygo.Channel[usage.ReadState]) error {
	t, log, e := s.get()
	if e != nil {
		return e
	}
	if err := pageLeft(ctx, t.Watch(ctx, states.Send)); err != nil {
		return wire(log, "Watch", err)
	}
	return nil
}
