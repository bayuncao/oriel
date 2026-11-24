package mygoipc

import (
	"context"
	"log/slog"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/integration"
)

// Integrations 是設定 › 集成的 IPC 服務（BindAs "Integrations"，D108）。
type Integrations struct{ late[*integration.Service] }

// ConnectIntegrations 給 Integrations 服務接上後端（D61：包級函式，不是頁面可呼叫的方法）。
func ConnectIntegrations(s *Integrations, svc *integration.Service, log *slog.Logger) {
	s.connect(svc, log)
}

// Refresh 檢測一次（打開頁面、點“重新檢測”時，IG25）：進行中時等它結束，不另起。
func (s *Integrations) Refresh(ctx context.Context) (integration.IntegrationCatalog, error) {
	svc, log, e := s.get()
	if e != nil {
		return integration.IntegrationCatalog{}, e
	}
	c, err := svc.Refresh(ctx)
	if err != nil {
		return integration.IntegrationCatalog{}, wire(log, "Refresh", err)
	}
	return c, nil
}

// Connect 先檢測，接得上就記下已連接；接不上回報原因（IG05、IG06）。
func (s *Integrations) Connect(ctx context.Context, id integration.IntegrationID) (integration.IntegrationCatalog, error) {
	svc, log, e := s.get()
	if e != nil {
		return integration.IntegrationCatalog{}, e
	}
	c, err := svc.Connect(ctx, id)
	if err != nil {
		return integration.IntegrationCatalog{}, wire(log, "Connect", err)
	}
	return c, nil
}

// Disconnect 記下不再使用；gh 的登入不變（IG08、IG24）。
func (s *Integrations) Disconnect(ctx context.Context, id integration.IntegrationID) (integration.IntegrationCatalog, error) {
	svc, log, e := s.get()
	if e != nil {
		return integration.IntegrationCatalog{}, e
	}
	c, err := svc.Disconnect(ctx, id)
	if err != nil {
		return integration.IntegrationCatalog{}, wire(log, "Disconnect", err)
	}
	return c, nil
}

// Watch 先推現在的內容，之後每次變化推一次（IG12）；頁面關掉 channel 或應用退出時結束。
func (s *Integrations) Watch(ctx context.Context, catalogs *mygo.Channel[integration.IntegrationCatalog]) error {
	svc, log, e := s.get()
	if e != nil {
		return e
	}
	if err := pageLeft(ctx, svc.Watch(ctx, catalogs.Send)); err != nil {
		return wire(log, "Watch", err)
	}
	return nil
}
