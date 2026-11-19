package provider

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 功能區右邊的 Provider keys（m2.md 第 24 節，D132、D134）：只存，不驗證。每把鑰匙有
// 名稱與廠商圖標，同一家可以存好幾把，可以另記一個 base URL（D136）。鑰匙存在系統鑰匙串，記錄
// （名稱、圖標、末四位、添加時間、base URL）存在資料庫。OrielHub 不拿鑰匙調用模型、不注入給 agent，
// 也不把它或 base URL 發往任何地方。

// StoredKey 是一把存下的鑰匙的記錄；鑰匙本身只在鑰匙串裡（PK15、PK16）。
type StoredKey struct {
	// ID 是這把鑰匙的識別，也是它在鑰匙串裡的名字的一部分。
	ID   string `json:"id"`
	Name string `json:"name"`
	// Icon 是 KeyIcons() 裡一家的 ID；空字串（舊記錄裡不在清單上的廠商）畫名稱首字母。
	Icon    string    `json:"icon"`
	Last4   string    `json:"last4"`
	AddedAt time.Time `json:"addedAt"`
	// BaseURL 是用戶記下的 API 地址（D136，PK20–PK36）；空字串 = 廠商的官方地址。
	// 存的是 NormalizeBaseURL 之後的樣子；OrielHub 不向它發請求。
	BaseURL string `json:"baseUrl"`
}

// KeyIcon 是鑰匙可以選的廠商圖標（PK05）：狀態卡片上的廠商，同一順序。
type KeyIcon struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// KeyIcons 是下拉裡的廠商圖標，依 Catalog() 的順序（PK05）。
func KeyIcons() []KeyIcon {
	vendors := Catalog()
	out := make([]KeyIcon, len(vendors))
	for i, v := range vendors {
		out[i] = KeyIcon{ID: v.ID, Name: v.Name}
	}
	return out
}

// KeyStore 存鑰匙的記錄（store 實作，測試用 MemKeyStore）。
type KeyStore interface {
	// Keys 回傳所有記錄，依添加的先後（同一時間依 ID）。
	Keys(ctx context.Context) ([]StoredKey, error)
	PutKey(ctx context.Context, k StoredKey) error
	// SetKeyBaseURL 只換掉一把的 base URL；沒有這一把時回 false。
	SetKeyBaseURL(ctx context.Context, id, baseURL string) (bool, error)
	// DeleteKey 刪除一把的記錄；沒有時不算錯。
	DeleteKey(ctx context.Context, id string) error
}

// Keychain 存鑰匙本身（app 以 mygo.Secrets 實作，測試用 MemKeychain）。
type Keychain interface {
	// Get 找不到時回 ErrNoSecret；沒有憑證儲存時回 errors.ErrUnsupported。
	Get(name string) ([]byte, error)
	Set(name string, value []byte) error
	// Delete 找不到時不算錯。
	Delete(name string) error
}

// ErrNoSecret：鑰匙串裡沒有這一項。
var ErrNoSecret = errors.New("provider: secret not found")

// secretName 是鑰匙在鑰匙串裡的名字。D134 之前的記錄以廠商為 ID（如 provider.key.anthropic），
// 遷移後名字不變。
func secretName(id string) string { return "provider.key." + id }

// KeyOptions 是 Keys 的依賴；Keychain 與 Store 必填。
type KeyOptions struct {
	Keychain Keychain
	Store    KeyStore
	// Now 是時鐘；nil → time.Now。
	Now func() time.Time
	// Logger 記錄失敗（不含鑰匙）；nil → 記錄時的 slog.Default()。
	Logger *slog.Logger
}

// Keys 存與刪鑰匙。操作依序進行（一把鎖）；方法可從任何 goroutine 呼叫。
type Keys struct {
	o  KeyOptions
	mu sync.Mutex
}

// NewKeys 回傳 Keys。
func NewKeys(o KeyOptions) *Keys {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Keys{o: o}
}

// Icons 是下拉裡的廠商圖標（PK05）。
func (k *Keys) Icons() []KeyIcon { return KeyIcons() }

// List 回傳存下的鑰匙，依添加的先後（PK03）；鑰匙串裡已經沒有的，連記錄一起刪掉（PK13）。
func (k *Keys) List(ctx context.Context) ([]StoredKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	list, err := k.o.Store.Keys(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		_, err := k.o.Keychain.Get(secretName(s.ID))
		switch {
		case errors.Is(err, ErrNoSecret):
			if err := k.o.Store.DeleteKey(ctx, s.ID); err != nil {
				return nil, err
			}
		case err != nil:
			// 鑰匙串暫時讀不了：記錄照列，不刪。
			k.logger().Warn("provider key not checked in the keychain", "key", s.ID, "err", err)
		}
	}
	return k.stored(ctx)
}

// Add 存下一把鑰匙（PK06）：不發請求，也不檢查鑰匙的格式。名稱與鑰匙去掉首尾空白後不能是空的
// （PK07），圖標要是 KeyIcons() 裡的一家，base URL 可以不填，填了要合 NormalizeBaseURL（PK21–PK25）。
// 數據庫寫不進時把鑰匙串裡的這一把刪掉（PK10）。
func (k *Keys) Add(ctx context.Context, name, icon, key, baseURL string) ([]StoredKey, error) {
	name, key = strings.TrimSpace(name), strings.TrimSpace(key)
	switch {
	case name == "":
		return nil, keyError(CodeKeyNameMissing, nil)
	case !slices.ContainsFunc(KeyIcons(), func(i KeyIcon) bool { return i.ID == icon }):
		return nil, keyError(CodeUnknownIcon, nil)
	case key == "":
		return nil, keyError(CodeKeyMissing, nil)
	}
	base, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	s := StoredKey{ID: strings.ToLower(rand.Text()), Name: name, Icon: icon, Last4: last4(key), AddedAt: k.o.Now(), BaseURL: base}
	if err := k.o.Keychain.Set(secretName(s.ID), []byte(key)); err != nil {
		k.logger().Warn("provider key not saved in the keychain", "key", s.ID, "err", err)
		return nil, keyError(CodeKeychainUnavailable, err, "detail", err.Error())
	}
	if err := k.o.Store.PutKey(ctx, s); err != nil {
		if err := k.o.Keychain.Delete(secretName(s.ID)); err != nil {
			k.logger().Warn("provider key not removed from the keychain", "key", s.ID, "err", err)
		}
		return nil, err
	}
	return k.stored(ctx)
}

// SetBaseURL 改一把鑰匙的 base URL（PK28–PK33）：只改資料庫裡的記錄，鑰匙串、末四位、添加時間與
// 順序不動；空白 = 清空（官方地址）。沒有這一把的記錄時回 CodeKeyNotFound。
func (k *Keys) SetBaseURL(ctx context.Context, id, baseURL string) ([]StoredKey, error) {
	base, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	found, err := k.o.Store.SetKeyBaseURL(ctx, id, base)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, keyError(CodeKeyNotFound, nil)
	}
	return k.stored(ctx)
}

// NormalizeBaseURL 檢查並整理 base URL（PK21–PK25）：去掉首尾空白，空的就是沒填（回空字串）；
// 否則要是帶主機名的 http 或 https 絕對地址：端口有寫就是 1–65535，不帶空白、用戶名密碼、查詢串或片段。
// 存的是解析後的樣子：scheme 與主機名小寫，去掉末尾的 /。不發請求，不檢查連不連得上。不合時回
// CodeInvalidBaseURL。
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	invalid := keyError(CodeInvalidBaseURL, nil)
	if strings.ContainsAny(raw, " \t\r\n#") {
		return "", invalid
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", invalid
		}
	}
	u.Host = strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/"), nil
}

// Remove 從鑰匙串與資料庫刪掉一把鑰匙（PK12）；沒有這一把時不算錯。
func (k *Keys) Remove(ctx context.Context, id string) ([]StoredKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.o.Keychain.Delete(secretName(id)); err != nil && !errors.Is(err, ErrNoSecret) {
		k.logger().Warn("provider key not deleted from the keychain", "key", id, "err", err)
		return nil, keyError(CodeKeychainUnavailable, err, "detail", err.Error())
	}
	if err := k.o.Store.DeleteKey(ctx, id); err != nil {
		return nil, err
	}
	return k.stored(ctx)
}

// stored 是全部的記錄，依添加的先後。
func (k *Keys) stored(ctx context.Context) ([]StoredKey, error) {
	list, err := k.o.Store.Keys(ctx)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []StoredKey{}
	}
	return list, nil
}

func last4(key string) string {
	r := []rune(key)
	if len(r) <= 4 {
		return ""
	}
	return string(r[len(r)-4:])
}

func (k *Keys) logger() *slog.Logger {
	if k.o.Logger != nil {
		return k.o.Logger
	}
	return slog.Default()
}

// MemKeyStore 是記在記憶體裡的 KeyStore（與 store 的實作跑同一份一致性測試）。
type MemKeyStore struct {
	mu   sync.Mutex
	keys map[string]StoredKey
}

func NewMemKeyStore() *MemKeyStore { return &MemKeyStore{keys: map[string]StoredKey{}} }

func (m *MemKeyStore) Keys(context.Context) ([]StoredKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StoredKey, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b StoredKey) int {
		if c := a.AddedAt.Compare(b.AddedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func (m *MemKeyStore) PutKey(_ context.Context, k StoredKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[k.ID] = k
	return nil
}

func (m *MemKeyStore) SetKeyBaseURL(_ context.Context, id, baseURL string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return false, nil
	}
	k.BaseURL = baseURL
	m.keys[id] = k
	return true, nil
}

func (m *MemKeyStore) DeleteKey(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, id)
	return nil
}

// MemKeychain 是記在記憶體裡的 Keychain，給測試用；Err 不為 nil 時每個操作都回它
// （模擬沒有憑證儲存或被拒絕存取，PK09）。
type MemKeychain struct {
	mu      sync.Mutex
	secrets map[string][]byte
	Err     error
}

func NewMemKeychain() *MemKeychain { return &MemKeychain{secrets: map[string][]byte{}} }

func (m *MemKeychain) Get(name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	v, ok := m.secrets[name]
	if !ok {
		return nil, ErrNoSecret
	}
	return slices.Clone(v), nil
}

func (m *MemKeychain) Set(name string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	if m.secrets == nil {
		m.secrets = map[string][]byte{}
	}
	m.secrets[name] = slices.Clone(value)
	return nil
}

func (m *MemKeychain) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	delete(m.secrets, name)
	return nil
}
