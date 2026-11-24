package integration

import (
	"context"
	"slices"
	"sync"
)

// MemStore 是記在記憶體裡的 Store（測試用；Err 非 nil 時讀寫都回傳它）。
type MemStore struct {
	mu  sync.Mutex
	ids []IntegrationID
	Err error
}

func (m *MemStore) EnabledIntegrations(context.Context) ([]IntegrationID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.ids), m.Err
}

func (m *MemStore) PutEnabledIntegrations(_ context.Context, ids []IntegrationID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	m.ids = slices.Clone(ids)
	return nil
}
