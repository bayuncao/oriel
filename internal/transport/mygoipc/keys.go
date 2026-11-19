package mygoipc

import (
	"context"
	"log/slog"

	"github.com/bayuncao/oriel/internal/provider"
)

// Keys 是功能區 Provider keys 的 IPC 服務（BindAs "Keys"，docs/architecture/provider.md）。
// 頁面只在添加時送出一次完整的鑰匙；回來的永遠只有名稱、圖標、末四位與 base URL（m2.md PK16）。
type Keys struct{ late[*provider.Keys] }

// ConnectKeys 給 Keys 服務接上後端（D61：包級函式，不是頁面可呼叫的方法）。
func ConnectKeys(s *Keys, k *provider.Keys, log *slog.Logger) { s.connect(k, log) }

// Icons 是添加鑰匙時下拉裡的廠商圖標（PK05）。
func (s *Keys) Icons() ([]provider.KeyIcon, error) {
	k, _, e := s.get()
	if e != nil {
		return nil, e
	}
	return k.Icons(), nil
}

// List 回傳存下的鑰匙，依添加的先後；鑰匙串裡已經沒有的不再列出（PK13）。
func (s *Keys) List(ctx context.Context) ([]provider.StoredKey, error) {
	k, log, e := s.get()
	if e != nil {
		return nil, e
	}
	list, err := k.List(ctx)
	if err != nil {
		return nil, wire(log, "List", err)
	}
	return list, nil
}

// Add 存下一把鑰匙，不驗證鑰匙（PK06–PK10）；baseURL 可以是空的（PK21–PK25）。回傳全部的鑰匙。
func (s *Keys) Add(ctx context.Context, name, icon, key, baseURL string) ([]provider.StoredKey, error) {
	k, log, e := s.get()
	if e != nil {
		return nil, e
	}
	list, err := k.Add(ctx, name, icon, key, baseURL)
	if err != nil {
		return nil, wire(log, "Add", err)
	}
	return list, nil
}

// SetBaseURL 改一把鑰匙的 base URL，空的 = 清空（PK28–PK33），回傳全部的鑰匙。
func (s *Keys) SetBaseURL(ctx context.Context, id, baseURL string) ([]provider.StoredKey, error) {
	k, log, e := s.get()
	if e != nil {
		return nil, e
	}
	list, err := k.SetBaseURL(ctx, id, baseURL)
	if err != nil {
		return nil, wire(log, "SetBaseURL", err)
	}
	return list, nil
}

// Remove 刪掉一把鑰匙（PK12），回傳全部的鑰匙。
func (s *Keys) Remove(ctx context.Context, id string) ([]provider.StoredKey, error) {
	k, log, e := s.get()
	if e != nil {
		return nil, e
	}
	list, err := k.Remove(ctx, id)
	if err != nil {
		return nil, wire(log, "Remove", err)
	}
	return list, nil
}
