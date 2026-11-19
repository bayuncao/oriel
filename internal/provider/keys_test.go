package provider

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type keysTest struct {
	*Keys
	keychain *MemKeychain
	store    *failingStore
	log      *bytes.Buffer
}

// failingStore 是會寫不進的資料庫（PK10、PK33）：putErr 不為 nil 時 PutKey 與 SetKeyBaseURL 回它。
type failingStore struct {
	*MemKeyStore
	putErr error
}

func (s *failingStore) PutKey(ctx context.Context, k StoredKey) error {
	if s.putErr != nil {
		return s.putErr
	}
	return s.MemKeyStore.PutKey(ctx, k)
}

func (s *failingStore) SetKeyBaseURL(ctx context.Context, id, baseURL string) (bool, error) {
	if s.putErr != nil {
		return false, s.putErr
	}
	return s.MemKeyStore.SetKeyBaseURL(ctx, id, baseURL)
}

func newKeysTest(t *testing.T) *keysTest {
	t.Helper()
	kt := &keysTest{keychain: NewMemKeychain(), store: &failingStore{MemKeyStore: NewMemKeyStore()}, log: &bytes.Buffer{}}
	kt.Keys = NewKeys(KeyOptions{Keychain: kt.keychain, Store: kt.store, Now: func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(kt.log, nil))})
	return kt
}

// secret 是鑰匙串裡這一把的值；沒有時是空字串。
func (kt *keysTest) secret(id string) string {
	b, err := kt.keychain.Get(secretName(id))
	if errors.Is(err, ErrNoSecret) {
		return ""
	}
	return string(b)
}

func (kt *keysTest) add(t *testing.T, name, icon, key string) StoredKey {
	t.Helper()
	before, _ := kt.store.Keys(context.Background())
	list, err := kt.Add(context.Background(), name, icon, key, "")
	if err != nil {
		t.Fatalf("Add(%q, %q): %v", name, icon, err)
	}
	if len(list) != len(before)+1 {
		t.Fatalf("Add returned %d keys, had %d", len(list), len(before))
	}
	for _, k := range list {
		if !slices.ContainsFunc(before, func(b StoredKey) bool { return b.ID == k.ID }) {
			return k
		}
	}
	t.Fatal("added key not in the list")
	return StoredKey{}
}

func codeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return string(e.Code)
	}
	return ""
}

// PK05：圖標是狀態卡片上的廠商，同一順序。
func TestKeyIcons(t *testing.T) {
	icons := KeyIcons()
	vendors := Catalog()
	if len(icons) != len(vendors) {
		t.Fatalf("%d icons, %d vendors", len(icons), len(vendors))
	}
	for i, v := range vendors {
		if icons[i] != (KeyIcon{ID: v.ID, Name: v.Name}) {
			t.Errorf("icon %d = %+v, want %s %s", i, icons[i], v.ID, v.Name)
		}
	}
}

// PK01、PK03、PK06、PK08：沒有鑰匙時是空的清單；添加不驗證，存進鑰匙串與記錄，依添加的先後列出，
// 同一家、同名都可以有好幾把。
func TestAddAndList(t *testing.T) {
	kt := newKeysTest(t)
	ctx := context.Background()
	list, err := kt.List(ctx)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("empty list = %#v, %v", list, err)
	}
	// 不檢查格式：中間有空白、不像任何一家的前綴都照存；首尾的空白去掉。
	work := kt.add(t, "  Work  ", "anthropic", "  not a real key 4f2a ")
	again := kt.add(t, "Work", "anthropic", "sk-ant-second-9c01")
	other := kt.add(t, "Personal", "deepseek", "abc")
	if work.Name != "Work" || work.Icon != "anthropic" || work.Last4 != "4f2a" || !work.AddedAt.Equal(now) || work.ID == "" {
		t.Errorf("work = %+v", work)
	}
	if got := kt.secret(work.ID); got != "not a real key 4f2a" {
		t.Errorf("keychain = %q", got)
	}
	if other.Last4 != "" {
		t.Errorf("a key of four characters or fewer shows no last four: %q", other.Last4)
	}
	if work.ID == again.ID || again.ID == other.ID {
		t.Errorf("ids not distinct: %s %s %s", work.ID, again.ID, other.ID)
	}
	list, err = kt.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list = %+v", list)
	}
}

// PK07：名稱或鑰匙是空的，或圖標不在清單裡，不存。
func TestAddRejects(t *testing.T) {
	for _, c := range []struct {
		name, icon, key, code string
	}{
		{"", "openai", "sk-1234", string(CodeKeyNameMissing)},
		{"   ", "openai", "sk-1234", string(CodeKeyNameMissing)},
		{"Work", "", "sk-1234", string(CodeUnknownIcon)},
		{"Work", "xai", "sk-1234", string(CodeUnknownIcon)}, // 不在狀態卡片上
		{"Work", "OpenAI", "sk-1234", string(CodeUnknownIcon)},
		{"Work", "openai", "", string(CodeKeyMissing)},
		{"Work", "openai", " \n\t", string(CodeKeyMissing)},
	} {
		kt := newKeysTest(t)
		_, err := kt.Add(context.Background(), c.name, c.icon, c.key, "")
		if codeOf(err) != c.code {
			t.Errorf("Add(%q, %q, %q) = %v, want %s", c.name, c.icon, c.key, err, c.code)
		}
		if list, _ := kt.store.Keys(context.Background()); len(list) != 0 {
			t.Errorf("Add(%q, %q, %q) stored %+v", c.name, c.icon, c.key, list)
		}
	}
}

// PK09、PK15：鑰匙串用不了時不存；鑰匙不進錯誤與日誌。
func TestAddKeychainUnavailable(t *testing.T) {
	kt := newKeysTest(t)
	kt.keychain.Err = errors.ErrUnsupported
	const key = "sk-secret-value-7777"
	_, err := kt.Add(context.Background(), "Work", "openai", key, "")
	if codeOf(err) != string(CodeKeychainUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if list, _ := kt.store.Keys(context.Background()); len(list) != 0 {
		t.Errorf("stored %+v", list)
	}
	if strings.Contains(err.Error(), key) || strings.Contains(kt.log.String(), key) {
		t.Errorf("key leaked: %v / %s", err, kt.log)
	}
}

// PK10：鑰匙串寫成了但資料庫寫不進：不存，鑰匙串裡也不留。
func TestAddStoreFails(t *testing.T) {
	kt := newKeysTest(t)
	kt.store.putErr = errors.New("disk full")
	if _, err := kt.Add(context.Background(), "Work", "openai", "sk-1234", ""); err == nil {
		t.Fatal("no error")
	}
	kt.keychain.mu.Lock()
	left := len(kt.keychain.secrets)
	kt.keychain.mu.Unlock()
	if left != 0 {
		t.Errorf("%d secrets left in the keychain", left)
	}
}

// PK12：刪掉一把，其他的不動；沒有這一把不算錯；鑰匙串用不了時記錄留著。
func TestRemove(t *testing.T) {
	kt := newKeysTest(t)
	ctx := context.Background()
	a := kt.add(t, "Work", "openai", "sk-aaaa")
	b := kt.add(t, "Personal", "openai", "sk-bbbb")
	list, err := kt.Remove(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != b.ID || kt.secret(a.ID) != "" || kt.secret(b.ID) != "sk-bbbb" {
		t.Fatalf("after remove: %+v, secret a %q", list, kt.secret(a.ID))
	}
	if _, err := kt.Remove(ctx, "missing"); err != nil {
		t.Fatalf("removing a missing key: %v", err)
	}
	kt.keychain.Err = errors.ErrUnsupported
	if _, err := kt.Remove(ctx, b.ID); codeOf(err) != string(CodeKeychainUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if list, _ := kt.store.Keys(ctx); len(list) != 1 {
		t.Errorf("record removed although the keychain failed: %+v", list)
	}
	kt.keychain.Err = nil
	list, err = kt.Remove(ctx, b.ID)
	if err != nil || len(list) != 0 {
		t.Fatalf("last key removed: %+v %v", list, err)
	}
}

// PK13：鑰匙串裡那一項已不在時，列出時連記錄一起刪；鑰匙串暫時讀不了時記錄照列。
func TestListDropsMissingSecrets(t *testing.T) {
	kt := newKeysTest(t)
	ctx := context.Background()
	gone := kt.add(t, "Work", "openai", "sk-aaaa")
	kept := kt.add(t, "Personal", "mistral", "sk-bbbb")
	if err := kt.keychain.Delete(secretName(gone.ID)); err != nil {
		t.Fatal(err)
	}
	list, err := kt.List(ctx)
	if err != nil || len(list) != 1 || list[0].ID != kept.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
	if records, _ := kt.store.Keys(ctx); len(records) != 1 {
		t.Errorf("record of the missing secret kept: %+v", records)
	}
	kt.keychain.Err = errors.ErrUnsupported
	list, err = kt.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("keychain unreadable: %+v %v", list, err)
	}
}

// 同時添加與刪除：每把各有自己的 ID，全部存下，記錄與鑰匙串一致。
func TestKeysConcurrent(t *testing.T) {
	kt := newKeysTest(t)
	ctx := context.Background()
	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if _, err := kt.Add(ctx, "Key", "openai", strings.Repeat("k", i+5), ""); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	list, err := kt.List(ctx)
	if err != nil || len(list) != n {
		t.Fatalf("list has %d keys, %v", len(list), err)
	}
	var wg2 sync.WaitGroup
	for _, k := range list {
		wg2.Go(func() {
			if _, err := kt.Remove(ctx, k.ID); err != nil {
				t.Error(err)
			}
		})
	}
	wg2.Wait()
	if list, _ := kt.List(ctx); len(list) != 0 {
		t.Fatalf("left %+v", list)
	}
	kt.keychain.mu.Lock()
	defer kt.keychain.mu.Unlock()
	if len(kt.keychain.secrets) != 0 {
		t.Errorf("%d secrets left", len(kt.keychain.secrets))
	}
}

// PK21–PK25：base URL 可以不填；填了去掉首尾空白與末尾的 /，scheme 與主機名小寫；要是帶主機名的
// http 或 https 地址，端口有寫就是 1–65535，不帶空白、用戶名密碼、查詢串或片段。
func TestNormalizeBaseURL(t *testing.T) {
	for _, c := range []struct {
		scenario, in, want, code string
	}{
		{"PK21 empty", "", "", ""},
		{"PK21 blank", " \t\n", "", ""},
		{"PK22 trimmed", "  https://api.example.com/v1/ ", "https://api.example.com/v1", ""},
		{"PK22 several slashes", "https://api.example.com//", "https://api.example.com", ""},
		{"PK22 no path", "https://api.example.com", "https://api.example.com", ""},
		{"PK23 local http with port", "http://localhost:8080", "http://localhost:8080", ""},
		{"PK23 ip", "http://127.0.0.1:11434/v1", "http://127.0.0.1:11434/v1", ""},
		{"PK23 ipv6", "http://[::1]:8080/", "http://[::1]:8080", ""},
		{"PK23 highest port", "https://x.com:65535", "https://x.com:65535", ""},
		{"PK22 case", "HTTPS://API.Example.COM/V1/", "https://api.example.com/V1", ""},
		{"PK22 escaped path kept", "https://x.com/a%2Fb", "https://x.com/a%2Fb", ""},
		{"PK24 no scheme", "api.example.com", "", string(CodeInvalidBaseURL)},
		{"PK24 other scheme", "ftp://x", "", string(CodeInvalidBaseURL)},
		{"PK24 no host", "https://", "", string(CodeInvalidBaseURL)},
		{"PK24 port only", "https://:8080", "", string(CodeInvalidBaseURL)},
		{"PK24 not a url", "not a url", "", string(CodeInvalidBaseURL)},
		{"PK24 space in host", "https://api .example.com", "", string(CodeInvalidBaseURL)},
		{"PK24 opaque", "https:api.example.com", "", string(CodeInvalidBaseURL)},
		{"PK24 space in path", "https://x.com/a b", "", string(CodeInvalidBaseURL)},
		{"PK24 tab in path", "https://x.com/\tv1", "", string(CodeInvalidBaseURL)},
		{"PK24 port too high", "https://x.com:99999", "", string(CodeInvalidBaseURL)},
		{"PK24 port zero", "https://x.com:0", "", string(CodeInvalidBaseURL)},
		{"PK24 empty port", "https://x.com:/v1", "", string(CodeInvalidBaseURL)},
		{"PK24 bad escape", "https://x.com/%zz", "", string(CodeInvalidBaseURL)},
		{"PK25 user and password", "https://user:pw@x.com", "", string(CodeInvalidBaseURL)},
		{"PK25 query", "https://x.com?a=1", "", string(CodeInvalidBaseURL)},
		{"PK25 empty query", "https://x.com/?", "", string(CodeInvalidBaseURL)},
		{"PK25 fragment", "https://x.com#a", "", string(CodeInvalidBaseURL)},
		{"PK25 empty fragment", "https://x.com/#", "", string(CodeInvalidBaseURL)},
	} {
		t.Run(c.scenario, func(t *testing.T) {
			got, err := NormalizeBaseURL(c.in)
			if got != c.want || codeOf(err) != c.code || (c.code == "") != (err == nil) {
				t.Errorf("NormalizeBaseURL(%q) = %q, %v; want %q, %s", c.in, got, err, c.want, c.code)
			}
		})
	}
}

// PK21、PK22、PK24：添加時存下整理過的 base URL；不合時不存，鑰匙串裡也沒有寫入。
func TestAddBaseURL(t *testing.T) {
	for _, c := range []struct {
		scenario, in, want, code string
	}{
		{"PK21 not filled", "", "", ""},
		{"PK22 normalized", " https://api.example.com/v1/ ", "https://api.example.com/v1", ""},
		{"PK24 invalid", "api.example.com", "", string(CodeInvalidBaseURL)},
	} {
		t.Run(c.scenario, func(t *testing.T) {
			kt := newKeysTest(t)
			list, err := kt.Add(context.Background(), "Work", "openai", "sk-1234", c.in)
			if codeOf(err) != c.code {
				t.Fatalf("err = %v, want %s", err, c.code)
			}
			if c.code != "" {
				kt.keychain.mu.Lock()
				left := len(kt.keychain.secrets)
				kt.keychain.mu.Unlock()
				if records, _ := kt.store.Keys(context.Background()); len(records) != 0 || left != 0 {
					t.Errorf("stored %+v, %d secrets", records, left)
				}
				return
			}
			if len(list) != 1 || list[0].BaseURL != c.want {
				t.Errorf("list = %+v, want base URL %q", list, c.want)
			}
		})
	}
}

// PK28–PK33：改 base URL 只改記錄；鑰匙串、末四位、添加時間與順序不動；清空存成空的；不合或
// 鑰匙已經不在時不改；資料庫寫不進時回那個錯。
func TestSetBaseURL(t *testing.T) {
	later := now.Add(time.Hour)
	for _, c := range []struct {
		scenario string
		start    string
		id       string // 空 = 第一把
		in       string
		putErr   error
		want     string
		code     string
		failed   bool
	}{
		{scenario: "PK28 set", in: "https://proxy.example.com/v1/", want: "https://proxy.example.com/v1"},
		{scenario: "PK28 change", start: "https://a.example.com", in: "http://localhost:4000", want: "http://localhost:4000"},
		{scenario: "PK29 clear", start: "https://a.example.com", in: "", want: ""},
		{scenario: "PK29 clear with blanks", start: "https://a.example.com", in: "   ", want: ""},
		{scenario: "PK30 invalid", start: "https://a.example.com", in: "ftp://x", want: "https://a.example.com", code: string(CodeInvalidBaseURL)},
		{scenario: "PK32 gone", start: "https://a.example.com", id: "missing", in: "https://b.example.com", want: "https://a.example.com", code: string(CodeKeyNotFound)},
		{scenario: "PK33 database fails", start: "https://a.example.com", in: "https://b.example.com", putErr: errors.New("disk full"), want: "https://a.example.com", failed: true},
	} {
		t.Run(c.scenario, func(t *testing.T) {
			kt := newKeysTest(t)
			ctx := context.Background()
			first, err := kt.Add(ctx, "Work", "openai", "sk-first-4f2a", c.start)
			if err != nil {
				t.Fatal(err)
			}
			kt.Keys.o.Now = func() time.Time { return later }
			second := kt.add(t, "Personal", "deepseek", "sk-second-9c01")
			before := first[0]
			id := before.ID
			if c.id != "" {
				id = c.id
			}
			kt.store.putErr = c.putErr
			list, err := kt.SetBaseURL(ctx, id, c.in)
			kt.store.putErr = nil
			switch {
			case c.failed && err == nil:
				t.Fatal("no error")
			case !c.failed && codeOf(err) != c.code:
				t.Fatalf("err = %v, want %s", err, c.code)
			case err == nil && len(list) != 2:
				t.Fatalf("list = %+v", list)
			}
			records, _ := kt.store.Keys(ctx)
			want := before
			want.BaseURL = c.want
			if len(records) != 2 || records[0] != want || records[1] != second {
				t.Errorf("records = %+v\nwant %+v, %+v", records, want, second)
			}
			if kt.secret(before.ID) != "sk-first-4f2a" {
				t.Errorf("keychain changed: %q", kt.secret(before.ID))
			}
		})
	}
}
