package app

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/proc"
)

// registryBody 是給 Registry 測試的小型索引：兩個條目。
const registryBody = `{"version":"1.0.0","agents":[
	{"id":"one","name":"One","version":"1.0.0","distribution":{"npx":{"package":"one@1.0.0"}}},
	{"id":"two","name":"Two","version":"2.0.0","distribution":{"uvx":{"package":"two==2.0.0"}}}
]}`

// logSink 讓日誌測試的 writer 可以與 hub 的 goroutine 共用。
type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logSink) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(b)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// registryApp 建一個真 app，其 Registry 指向 httptest 伺服器。
func registryApp(t *testing.T, srv *httptest.Server, log *slog.Logger) (*App, string) {
	t.Helper()
	data := t.TempDir()
	a, err := New(ctxb2(), Options{DataDir: data, Inherited: proc.Env{"PATH=/usr/bin"},
		Root: t.TempDir(), RegistryURL: srv.URL, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, data
}

func registryServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(registryBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// AR1：無快取、伺服器在線 → 條目、確認時間，沒有 Stale。
func TestAgentsRegistry(t *testing.T) {
	srv := registryServer(t)
	a, _ := registryApp(t, srv, nil)
	lst, err := a.Agents().Registry(ctxb2(), false)
	if err != nil || len(lst.Listings) != 2 || lst.Stale.Code != "" || lst.FetchedAt.IsZero() {
		t.Fatalf("%d listings, stale %v, fetched %v, %v", len(lst.Listings), lst.Stale, lst.FetchedAt, err)
	}
}

// AR2：確認過一次後伺服器消失 → 回傳快取，Stale 帶原因，
// FetchedAt 還是上次確認的時間。
func TestAgentsRegistryOffline(t *testing.T) {
	srv := registryServer(t)
	a, _ := registryApp(t, srv, nil)
	fresh, err := a.Agents().Registry(ctxb2(), false)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	stale, err := a.Agents().Registry(ctxb2(), true)
	if err != nil || len(stale.Listings) != len(fresh.Listings) ||
		stale.Stale.Code != agent.CodeRegistryUnreachable ||
		stale.Stale.Params["detail"] == "" ||
		!stale.FetchedAt.Equal(fresh.FetchedAt) {
		t.Fatalf("stale %v, fetched %v vs %v, %v",
			stale.Stale, stale.FetchedAt, fresh.FetchedAt, err)
	}
}

// AR3：快取目錄不可寫 → 仍回傳條目且沒有錯誤，只記下
// agent.registry_cache_write 的警告；再一次也一樣。
func TestAgentsRegistryCacheWrite(t *testing.T) {
	srv := registryServer(t)
	sink := &logSink{}
	log := slog.New(slog.NewTextHandler(sink, nil))
	a, data := registryApp(t, srv, log)
	dir := filepath.Join(data, "registry")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	lst, err := a.Agents().Registry(ctxb2(), true)
	if err != nil || len(lst.Listings) != 2 {
		t.Fatalf("%d listings, %v", len(lst.Listings), err)
	}
	if !strings.Contains(sink.String(), string(agent.CodeRegistryCacheWrite)) {
		t.Fatalf("log lacks %s: %q", agent.CodeRegistryCacheWrite, sink.String())
	}
	if _, err := a.Agents().Registry(ctxb2(), true); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

// AR4：無快取且伺服器連不上 → agent.registry_unreachable。
func TestAgentsRegistryUnreachable(t *testing.T) {
	srv := registryServer(t)
	a, _ := registryApp(t, srv, nil)
	srv.Close()
	_, err := a.Agents().Registry(ctxb2(), true)
	var p *agent.Problem
	if !errors.As(err, &p) || p.Code != agent.CodeRegistryUnreachable {
		t.Fatalf("%v", err)
	}
}
