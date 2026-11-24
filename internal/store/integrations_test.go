package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bayuncao/oriel/internal/integration"
)

var _ integration.Store = (*Store)(nil)

// TestEnabledIntegrations：沒存過是空的；存下的 ID 重開後還在；可以清空（IG05、IG08）。
func TestEnabledIntegrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.sqlite")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ids, err := s.EnabledIntegrations(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("fresh: %v %v", ids, err)
	}
	if err := s.PutEnabledIntegrations(ctx, []integration.IntegrationID{integration.GitHub}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if ids, err := s.EnabledIntegrations(ctx); err != nil || !slices.Equal(ids, []integration.IntegrationID{integration.GitHub}) {
		t.Fatalf("reopened: %v %v", ids, err)
	}
	if err := s.PutEnabledIntegrations(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.EnabledIntegrations(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("cleared: %v %v", ids, err)
	}
	// 通知等設定在同一張表，互不影響。
	if _, err := s.Settings(ctx); err != nil {
		t.Fatal(err)
	}
}
