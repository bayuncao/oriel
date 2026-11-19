package app

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/runtime/acp"
	"github.com/bayuncao/oriel/internal/store"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport/remote"
	"github.com/bayuncao/oriel/internal/usage"
	"github.com/bayuncao/oriel/internal/workspace"
)

// Options 是 App 的全部依賴（DataDir 必填；其餘可缺省）。
type Options struct {
	DataDir   string       // orielhub.db、registry/ 快取、worktrees/
	HTTP      *http.Client // Registry 與廠商狀態頁用；nil → app 建帶 proxy 的
	Secret    func(key string) (string, error)
	Notifier  hub.Notifier // nil → 不發通知
	Logger    *slog.Logger
	Inherited proc.Env // nil → proc.Inherited()；測試傳受控環境
	Root      string   // "" → /；僅測試

	RegistryURL string // "" → agent.RegistryURL；僅測試

	// Version 是 OrielHub 的版本（main 傳 mygo.App.Version()），填在 ACP 的 clientInfo。
	Version string

	// OnResume 登記電腦醒來時呼叫的函式（main 傳 mygo.Power.OnResume）；nil 不理會
	// 睡眠。讀本機用量的排程據此按牆鐘重排。
	OnResume func(fn func()) (off func())
	// UsageSources 是讀本機用量記錄的 Source；nil → 各內建 agent 的。僅測試。
	UsageSources []usage.Source
	// ProviderVendors 是功能區要讀狀態頁的廠商；nil → provider.Catalog()。僅測試。
	ProviderVendors []provider.Vendor
	// Keychain 存 Provider keys 的鑰匙與遠程的 Mac 密鑰、房間密鑰（main 傳 Keychain{}，即
	// mygo.Secrets）；nil → 存不了，回 provider.keychain_unavailable、remote.keychain_unavailable。
	Keychain provider.Keychain
	// Remote 為真時建立遠程（D145）；正式版先不建（D149，DV17），main 以 RemoteAvailable 決定。
	Remote bool
}

// App 是組合根：store、runner、discovery、workspace、hub、本機用量、廠商狀態、鑰匙與遠程。
type App struct {
	hub    *hub.Hub
	remote *remote.Control
	agents *Agents
	usage  *usage.Tracker
	status *provider.Monitor
	keys   *provider.Keys
	db     *store.Store
	runner *proc.Runner
	disc   *agent.Discovery
	closed atomic.Bool
}

// New 打開一切；任何失敗都把已打開的關掉。
func New(ctx context.Context, o Options) (*App, error) {
	if o.Inherited == nil {
		o.Inherited = proc.Inherited()
	}
	db, err := store.Open(ctx, filepath.Join(o.DataDir, "orielhub.db"),
		store.Options{})
	if err != nil {
		return nil, err
	}
	// 失敗時把已建立的都關掉（hub、discovery、runner、store）。
	var toClose []func()
	fail := func(err error) (*App, error) {
		for i := len(toClose) - 1; i >= 0; i-- {
			toClose[i]()
		}
		db.Close()
		return nil, err
	}
	runner := &proc.Runner{}
	toClose = append(toClose, runner.Close)
	disc := agent.NewDiscovery(agent.DiscoveryOptions{
		Runner: runner, Root: o.Root, Inherited: o.Inherited})
	log := cmp.Or(o.Logger, slog.Default())
	defs := catalog(log) // 唯讀，Discovery、Launcher 與 Agents 共用
	disc.SetAgents(defs, nil)
	toClose = append(toClose, disc.Close)
	git := workspace.New(workspace.Options{
		Runner: runner, Env: disc.Env, Root: o.Root})
	acpRuntime := acp.New(acp.Options{Runner: runner, Version: o.Version, Logger: o.Logger})
	h := hub.New(hub.Options{
		Log: db, Config: db, Index: db,
		Notifier: o.Notifier,
		Runtimes: map[task.Protocol]hub.Runtime{
			task.ProtocolACP: acpRuntime,
		},
		Launcher:     newLauncher(disc, defs, acpRuntime.Capabilities, log),
		Git:          git,
		Env:          disc.Env,
		Secret:       o.Secret,
		WorktreeRoot: filepath.Join(o.DataDir, "worktrees"),
		Logger:       o.Logger,
	})
	toClose = append(toClose, h.Close)
	if err := h.Start(ctx); err != nil {
		return fail(err)
	}
	disc.Start()
	sources := o.UsageSources
	if sources == nil {
		sources = usageSources()
	}
	tracker := usage.New(usage.Options{Sources: sources, Store: db, Env: disc.Env,
		OnResume: o.OnResume, Logger: o.Logger})
	tracker.Start()
	client := registryHTTP(o, disc)
	status := provider.New(provider.Options{HTTP: client, UserAgent: "OrielHub/" + cmp.Or(o.Version, "dev"),
		Vendors: o.ProviderVendors, Zone: usage.SystemZone, OnResume: o.OnResume, Logger: o.Logger})
	keychain := o.Keychain
	if keychain == nil {
		keychain = &provider.MemKeychain{Err: errors.ErrUnsupported}
	}
	keys := provider.NewKeys(provider.KeyOptions{Keychain: keychain, Store: db, Logger: o.Logger})
	agents := &Agents{disc: disc, defs: defs, http: client,
		regDir: filepath.Join(o.DataDir, "registry"), url: o.RegistryURL,
		root: o.Root, log: log}
	// 遠程（D145）：開關與中繼地址記在資料目錄，密鑰在鑰匙串；開著時在背景連中繼。
	// 正式版不建（D149）：不連中繼，也不讀鑰匙串。
	var rc *remote.Control
	if o.Remote {
		rc = remote.NewControl(remote.ControlOptions{Hub: h, Agents: agents, Usage: tracker,
			Secrets: remoteSecrets{keychain}, Path: filepath.Join(o.DataDir, "remote.json"),
			HTTPClient: client, Logger: log})
		if err := rc.Start(); err != nil {
			log.Warn("remote settings unreadable; remote stays off", "err", err)
		}
	}
	return &App{
		hub: h, remote: rc, db: db, runner: runner, disc: disc, usage: tracker, status: status, keys: keys,
		agents: agents,
	}, nil
}

// remoteSecrets 把 provider.Keychain 接成 remote.Secrets：找不到是 ok == false。
type remoteSecrets struct{ k provider.Keychain }

func (s remoteSecrets) Get(name string) ([]byte, bool, error) {
	b, err := s.k.Get(name)
	if errors.Is(err, provider.ErrNoSecret) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (s remoteSecrets) Set(name string, value []byte) error { return s.k.Set(name, value) }

// Close 關閉一切（冪等）：遠程 → 廠商狀態 → 本機用量 → hub → discovery → runner → store。
func (a *App) Close() {
	if !a.closed.CompareAndSwap(false, true) {
		return
	}
	if a.remote != nil {
		a.remote.Close()
	}
	a.status.Close()
	a.usage.Close()
	a.hub.Close()
	a.disc.Close()
	a.runner.Close()
	a.db.Close()
}

func (a *App) Hub() *hub.Hub                { return a.hub }
func (a *App) Usage() *usage.Tracker        { return a.usage }
func (a *App) Providers() *provider.Monitor { return a.status }
func (a *App) Keys() *provider.Keys         { return a.keys }

// Remote 是遠程的控制器；Options.Remote 為假時是 nil。
func (a *App) Remote() *remote.Control              { return a.remote }
func (a *App) Agents() *Agents                      { return a.agents }
func (a *App) Changes() <-chan []agent.Installation { return a.disc.Changes() }

// registryHTTP 建 Registry 的 HTTP client：proxy 從當時的
// 環境快照讀（拿不到就用 Inherited，不阻塞請求）。
func registryHTTP(o Options, disc *agent.Discovery) *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		env := o.Inherited
		if cur := disc.CurrentEnv(); cur != nil {
			env = cur
		}
		return proxyOf(env)(req.URL)
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}
}

// proxyOf 從一份環境快照建 proxy 函式（ALL_PROXY 兩邊兜底）。
func proxyOf(env proc.Env) func(*url.URL) (*url.URL, error) {
	get := func(k string) string {
		if v := env.Get(k); v != "" {
			return v
		}
		return env.Get(strings.ToLower(k))
	}
	return func(u *url.URL) (*url.URL, error) {
		https, httpP := get("HTTPS_PROXY"), get("HTTP_PROXY")
		if https == "" {
			https = get("ALL_PROXY")
		}
		if httpP == "" {
			httpP = get("ALL_PROXY")
		}
		cfg := httpproxy.Config{HTTPProxy: httpP, HTTPSProxy: https,
			NoProxy: get("NO_PROXY")}
		return cfg.ProxyFunc()(u)
	}
}
