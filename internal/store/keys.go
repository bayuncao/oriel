package store

import (
	"context"

	"github.com/bayuncao/oriel/internal/provider"
)

// Provider keys 的記錄（provider.KeyStore，docs/architecture/store.md）：名稱、廠商圖標、
// 末四位、添加時間與 base URL（D136）。鑰匙本身在系統鑰匙串，從不寫進資料庫（m2.md PK15）。

// Keys 回傳所有記錄，依添加的先後（同一時間依 ID）。
func (s *Store) Keys(ctx context.Context) ([]provider.StoredKey, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx, "SELECT id, name, icon, last4, added_at, base_url FROM provider_keys ORDER BY added_at, id")
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]provider.StoredKey, 0, len(rows.Values))
	for _, v := range rows.Values {
		at, err := parseOptionalAt(toStr(v[4]))
		if err != nil {
			return nil, err
		}
		out = append(out, provider.StoredKey{ID: toStr(v[0]), Name: toStr(v[1]), Icon: toStr(v[2]), Last4: toStr(v[3]), AddedAt: at, BaseURL: toStr(v[5])})
	}
	return out, nil
}

// PutKey 寫入或換掉一把的記錄。
func (s *Store) PutKey(ctx context.Context, k provider.StoredKey) error {
	if err := s.check(); err != nil {
		return err
	}
	_, err := s.w.Execute(ctx,
		"INSERT OR REPLACE INTO provider_keys(id, name, icon, last4, added_at, base_url) VALUES(?,?,?,?,?,?)",
		k.ID, k.Name, k.Icon, k.Last4, formatAt(k.AddedAt), k.BaseURL)
	return mapErr(err)
}

// SetKeyBaseURL 只換掉一把的 base URL；沒有這一把時回 false。
func (s *Store) SetKeyBaseURL(ctx context.Context, id, baseURL string) (bool, error) {
	if err := s.check(); err != nil {
		return false, err
	}
	r, err := s.w.Execute(ctx, "UPDATE provider_keys SET base_url = ? WHERE id = ?", baseURL, id)
	if err != nil {
		return false, mapErr(err)
	}
	return r.Changes > 0, nil
}

// DeleteKey 刪除一把的記錄；沒有時不算錯。
func (s *Store) DeleteKey(ctx context.Context, id string) error {
	if err := s.check(); err != nil {
		return err
	}
	_, err := s.w.Execute(ctx, "DELETE FROM provider_keys WHERE id = ?", id)
	return mapErr(err)
}

var _ provider.KeyStore = (*Store)(nil)
