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
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/integration"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/runtime/acp"
	"github.com/bayuncao/oriel/internal/store"
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

// App 是組合根：store、runner、discovery、workspace、hub、本機用量、廠商狀態、鑰匙、集成與遠程。
type App struct {
	hub    *hub.Hub
	remote *remote.Control
	agents *Agents
	usage  *usage.Tracker
	status *provider.Monitor
	keys   *provider.Keys
	integ  *integration.Service
	db     *store.Store
	runner *proc.Runner
	disc   *agent.Discovery
	// changes 是存下之後轉交的發現結果（saveInstallations）；Close 等它結束才關 store。
	changes *relay
	// sweeper 在刪除 Task 之後清理待清理的密鑰；Close 先停它（它用 hub 與 store）。
	sweeper *sweeper
	closed  atomic.Bool
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
	var changes *relay
	fail := func(err error) (*App, error) {
		for i := len(toClose) - 1; i >= 0; i-- {
			toClose[i]()
		}
		if changes != nil { // discovery 已關，轉交發現結果的 goroutine 會結束；等它寫完再關 store
			<-changes.done
		}
		db.Close()
		return nil, err
	}
	runner := &proc.Runner{}
	toClose = append(toClose, runner.Close)
	// 使用者的目錄設定與上一次的發現結果（m4.md AC02、AC05）。
	cfg, err := db.AgentConfig(ctx)
	if err != nil {
		return fail(err)
	}
	last, err := db.LastInstallations(ctx)
	if err != nil {
		return fail(err)
	}
	disc := agent.NewDiscovery(agent.DiscoveryOptions{
		Runner: runner, Root: o.Root, Inherited: o.Inherited, Previous: last})
	log := cmp.Or(o.Logger, slog.Default())
	// 用量先建好（Start 在 hub 之後）：Claude 的轉接器把協議報來的額度交給它（D87）。
	sources := o.UsageSources
	if sources == nil {
		sources = usageSources()
	}
	tracker := usage.New(usage.Options{Sources: sources, Store: db, Env: disc.Env,
		OnResume: o.OnResume, Logger: o.Logger})
	toClose = append(toClose, tracker.Close)
	acpRuntime := acp.New(acp.Options{Runner: runner, Version: o.Version, Logger: o.Logger})
	runtimes := newRuntimes(runner, o.Version, o.Logger, acpRuntime, tracker.Report)
	keychain := o.Keychain
	if keychain == nil {
		keychain = &provider.MemKeychain{Err: errors.ErrUnsupported}
	}
	client := registryHTTP(o, disc)
	agents := &Agents{disc: disc, builtins: builtins(), runtimes: runtimes, store: db, keychain: keychain,
		http: client, regDir: filepath.Join(o.DataDir, "registry"), url: o.RegistryURL,
		root: o.Root, log: log, now: time.Now}
	agents.load(cfg)
	toClose = append(toClose, disc.Close)
	sweeps := newSweeper()
	launch := newLauncher(disc, agents, acpRuntime.Capabilities, log)
	// 解析放掉的密鑰在待清理裡時再清理一次：清理時它被 lease 跳過了，建不成 Task 時沒有別的事會再觸發（AC37）。
	launch.released = func(keys []string) {
		if agents.retiredAny(keys) {
			sweeps.trigger()
		}
	}
	git := workspace.New(workspace.Options{
		Runner: runner, Env: disc.Env, Root: o.Root})
	h := hub.New(hub.Options{
		Log: db, Config: db, Index: db,
		Notifier:     o.Notifier,
		Runtimes:     runtimes,
		Launcher:     launch,
		Git:          git,
		Env:          disc.Env,
		Secret:       o.Secret,
		WorktreeRoot: WorktreeRoot(o.DataDir),
		Logger:       o.Logger,
	})
	toClose = append(toClose, h.Close)
	if err := h.Start(ctx); err != nil {
		return fail(err)
	}
	agents.forget = h.ForgetAgent
	agents.refs, agents.leased = h.SecretsInUse, launch.Leased
	disc.Start()
	changes = saveInstallations(disc.Changes(), db, log)
	tracker.Start()
	status := provider.New(provider.Options{HTTP: client, UserAgent: "OrielHub/" + cmp.Or(o.Version, "dev"),
		Vendors: o.ProviderVendors, Zone: usage.SystemZone, OnResume: o.OnResume, Logger: o.Logger})
	keys := provider.NewKeys(provider.KeyOptions{Keychain: keychain, Store: db, Logger: o.Logger})
	// 集成（D108）：用與 agent 相同的環境快照找 gh；啟動時檢測一次（IG15）。
	integ, err := integration.New(ctx, integration.Options{Runner: runner, Env: disc.Env, Store: db, Root: o.Root})
	if err != nil {
		status.Close()
		return fail(err)
	}
	integ.Start()
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
	// 待清理的密鑰（m4.md AC39）：啟動時在背景清理一次（不擋啟動），之後每刪一個 Task、每放掉一個引用待清理密鑰的解析清理一次。
	sweeps.start(h, agents, log)
	return &App{
		hub: h, remote: rc, db: db, runner: runner, disc: disc, usage: tracker, status: status, keys: keys,
		integ: integ, agents: agents, changes: changes, sweeper: sweeps,
	}, nil
}

// hubCode 是 err 的 hub 錯誤代碼；不是 hub.Error 時為 ""。
func hubCode(err error) hub.Code {
	if he, ok := errors.AsType[*hub.Error](err); ok {
		return he.Code
	}
	return ""
}

// sweeper 在背景清理待清理的密鑰（m4.md AC37、AC39）：trigger 只扣下一格（已經有一次在排時不再加），清理在自己的
// goroutine 跑，接連的觸發合成幾次。stop 讓它結束並等它。
type sweeper struct {
	due    chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newSweeper 先建好觸發的那一格（launcher 建好就能觸發）；start 之前的觸發等到 start 後才清理。
func newSweeper() *sweeper {
	ctx, cancel := context.WithCancel(context.Background())
	return &sweeper{due: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
}

func (s *sweeper) trigger() {
	select {
	case s.due <- struct{}{}:
	default: // 已經有一次在排
	}
}

func (s *sweeper) stop() {
	s.cancel()
	s.wg.Wait()
}

// start 先排一次（啟動時的清理），再看 Task 列表：推來刪除（TaskView.Removed）時要清理（AC39）。跟不上
// （hub.lagging）時可能漏了刪除，也要清理，再重新看。看的 goroutine 只觸發，不等清理，所以不會因清理慢而
// 跟不上。hub 關了時不再看；stop 時都結束。
func (s *sweeper) start(h *hub.Hub, agents *Agents, log *slog.Logger) {
	s.trigger()
	s.wg.Go(func() {
		for {
			err := h.WatchTasks(s.ctx, hub.LocalOwner, func(v hub.TaskView) error {
				if v.Removed {
					s.trigger()
				}
				return nil
			})
			if s.ctx.Err() != nil || hubCode(err) != hub.CodeLagging {
				if err != nil && !errors.Is(err, hub.ErrClosed) {
					log.Warn("app: watching tasks for retired secrets stopped", "err", err)
				}
				return
			}
			s.trigger()
		}
	})
	s.wg.Go(func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-s.due:
				agents.sweep(s.ctx)
			}
		}
	})
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

// Close 關閉一切（冪等）：遠程 → 集成 → 廠商狀態 → 本機用量 → hub → discovery → runner → store。
func (a *App) Close() {
	if !a.closed.CompareAndSwap(false, true) {
		return
	}
	if a.remote != nil {
		a.remote.Close()
	}
	a.sweeper.stop()
	a.integ.Close()
	a.status.Close()
	// hub 先關：它結束 Claude 的行程時報來的最後一份額度，usage 還收得下（D156）。
	a.hub.Close()
	a.usage.Close()
	a.disc.Close()
	<-a.changes.done
	a.runner.Close()
	a.db.Close()
}

func (a *App) Hub() *hub.Hub                { return a.hub }
func (a *App) Usage() *usage.Tracker        { return a.usage }
func (a *App) Providers() *provider.Monitor { return a.status }
func (a *App) Keys() *provider.Keys         { return a.keys }

// Integrations 是設定 › 集成的後端（D108）。
func (a *App) Integrations() *integration.Service { return a.integ }

// Remote 是遠程的控制器；Options.Remote 為假時是 nil。
func (a *App) Remote() *remote.Control              { return a.remote }
func (a *App) Agents() *Agents                      { return a.agents }
func (a *App) Changes() <-chan []agent.Installation { return a.changes.out }

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

// WorktreeRoot 是資料目錄 dataDir 裡放任務工作樹的目錄。
func WorktreeRoot(dataDir string) string { return filepath.Join(dataDir, "worktrees") }
