package store

import (
	"context"

	"github.com/bayuncao/oriel/internal/integration"
)

// integrationsKey 是設定表裡已連接的集成（D108）：只有 ID，沒有 token。
const integrationsKey = "integrations"

// integrationsRow 是 integrationsKey 的值。
type integrationsRow struct {
	Enabled []integration.IntegrationID `json:"enabled"`
}

// EnabledIntegrations 回傳已連接的集成；沒存過時是空的（integration.Store）。
func (s *Store) EnabledIntegrations(ctx context.Context) ([]integration.IntegrationID, error) {
	var row integrationsRow
	if _, err := s.getSetting(ctx, integrationsKey, &row); err != nil {
		return nil, err
	}
	return row.Enabled, nil
}

// PutEnabledIntegrations 換掉已連接的集成（integration.Store）。
func (s *Store) PutEnabledIntegrations(ctx context.Context, ids []integration.IntegrationID) error {
	return s.putSetting(ctx, integrationsKey, integrationsRow{Enabled: ids})
}
