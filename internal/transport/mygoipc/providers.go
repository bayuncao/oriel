package mygoipc

import (
	"context"
	"log/slog"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/provider"
)

// Providers 是功能區 AI 廠商狀態的 IPC 服務（BindAs "Providers"，docs/architecture/provider.md）。
type Providers struct{ late[*provider.Monitor] }

// ConnectProviders 給 Providers 服務接上後端（D61：包級函式，不是頁面可呼叫的方法）。
func ConnectProviders(s *Providers, m *provider.Monitor, log *slog.Logger) { s.connect(m, log) }

// Refresh 讀一次各家的狀態頁：進行中時等它讀完，不另讀（PS21）。頁面離開只結束
// 等待，讀取照常進行。
func (s *Providers) Refresh(ctx context.Context) (provider.Board, error) {
	m, log, e := s.get()
	if e != nil {
		return provider.Board{}, e
	}
	b, err := m.Refresh(ctx)
	if err != nil {
		return provider.Board{}, wire(log, "Refresh", err)
	}
	return b, nil
}

// Watch 先推上次的結果，之後每次變化推一次；頁面關掉 channel（離開概覽頁）或應用
// 退出時結束。有頁面在 Watch 時，電腦醒來會立刻讀一次（PS26）。
func (s *Providers) Watch(ctx context.Context, boards *mygo.Channel[provider.Board]) error {
	m, log, e := s.get()
	if e != nil {
		return e
	}
	if err := pageLeft(ctx, m.Watch(ctx, boards.Send)); err != nil {
		return wire(log, "Watch", err)
	}
	return nil
}
