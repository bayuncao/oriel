package app

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/task"
)

// Agents 是 Agent 目錄（m4.md 第 2 節）：內建的 Definition 加上使用者的設定（加入的 Registry 條目、
// 自訂 agent、覆蓋路徑，存在 store），合併成的目錄在執行中可變（AC04）。目錄換掉時 Discovery 重新探測，
// launcher 與頁面讀的都是同一份。也包著 Registry 快取，供 transport 使用。
type Agents struct {
	disc     *agent.Discovery
	builtins []agent.Definition
	runtimes map[task.Protocol]hub.Runtime
	store    agent.ConfigStore
	keychain provider.Keychain
	// forget 刪掉被移除的 agent 記下的模型（hub.ForgetAgent，AC22）；hub 建好後由 New 設。
	forget func(context.Context, task.AgentID) error
	// refs 回傳用過 agent 的 Task 的 Session 快照引用的密鑰（taskSecrets）；leased 回報密鑰是否被
	// 還沒寫進事件流的解析引用（launcher.Leased）。hub 建好後由 New 設；沒設時不清理（AC37）。
	refs    func(context.Context, task.AgentID) (map[string]bool, error)
	leased  func(key string) bool
	sweepMu sync.Mutex // 一次只跑一個清理
	// swept 在每次清理結束時呼叫；僅測試（確定性地等到刪除 Task 之後的清理）。
	swept  func()
	http   *http.Client
	regDir string
	url    string       // Registry 索引的位址；"" → agent.RegistryURL
	root   string       // 檔案系統根；僅測試
	log    *slog.Logger // nil → slog.Default()
	now    func() time.Time

	write sync.Mutex // 序列化設定的寫入：存好、換掉目錄、交給 Discovery 是一步
	mu    sync.Mutex // 保護下面三個；讀的人拿到的切片不再被改
	cfg   agent.Config
	defs  []agent.Definition
	probs []agent.CatalogError
}

// load 由存下的設定組出目錄（AC02、AC03），並交給 Discovery。
func (g *Agents) load(cfg agent.Config) {
	g.write.Lock()
	defer g.write.Unlock()
	g.apply(cfg)
}

// apply 由設定 cfg 組出目錄、換掉目前的，再交給 Discovery。呼叫者持有 write。
func (g *Agents) apply(cfg agent.Config) {
	defs, probs := catalog(g.log, g.runtimes, g.builtins, cfg)
	g.mu.Lock()
	g.cfg, g.defs, g.probs = cfg, defs, probs
	g.mu.Unlock()
	g.disc.SetAgents(defs, cfg.Overrides)
}

func (g *Agents) Installations() []agent.Installation { return g.disc.Installations() }
func (g *Agents) Refresh(ctx context.Context) error   { return g.disc.Refresh(ctx) }

// Definitions 是目前的目錄（唯讀）。
func (g *Agents) Definitions() []agent.Definition {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.defs
}

// Lookup 是目錄裡的 agent id 與它的 Profile pid（空著是預設的，PO18），在同一份目錄與設定裡取
// （launcher 解析時用）：不在目錄裡是 agent.unknown_agent（P09），沒有這個 Profile 是 agent.unknown_profile（PO17）。
func (g *Agents) Lookup(id task.AgentID, pid task.ProfileID) (agent.Definition, agent.Profile, error) {
	g.mu.Lock()
	defs, cfg := g.defs, g.cfg
	g.mu.Unlock()
	i := slices.IndexFunc(defs, func(d agent.Definition) bool { return d.ID == id })
	if i < 0 {
		return agent.Definition{}, agent.Profile{}, agent.ErrUnknownAgent
	}
	p, err := cfg.Profile(id, pid)
	if err != nil {
		return agent.Definition{}, agent.Profile{}, err
	}
	return defs[i], p, nil
}

// Problems 是設定裡進不了目錄的 agent 與原因（AC03）；頁面列出它們，可以移除。
func (g *Agents) Problems() []agent.CatalogError {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.probs)
}

// Rows 是 Agent 頁的列：目錄、發現結果，接上覆蓋路徑、Registry 條目的版本與自訂 agent 的設定。
func (g *Agents) Rows() []agent.AgentRow {
	g.mu.Lock()
	defs, cfg := g.defs, g.cfg
	g.mu.Unlock()
	return cfg.Annotate(agent.Rows(defs, g.disc.Installations()))
}

// Registry 取 Registry 列表（唯讀；force 重新確認）。索引取得但
// 快取寫入失敗時仍回傳列表，只記下警告。
func (g *Agents) Registry(ctx context.Context, force bool) (agent.RegistryList, error) {
	idx, err := g.index(ctx, force)
	if err != nil {
		return agent.RegistryList{}, err
	}
	env, err := g.disc.Env(ctx)
	if err != nil {
		return agent.RegistryList{}, err
	}
	installed := agent.FindInstalled(env, g.root, idx)
	g.mu.Lock()
	added := g.cfg.Registry
	g.mu.Unlock()
	return agent.RegistryList{
		Listings:  agent.ListRegistry(idx, g.builtins, added, installed),
		FetchedAt: idx.FetchedAt,
		Stale:     idx.Stale,
	}, nil
}

// index 取 Registry 索引；離線時是快取（AC18）。
func (g *Agents) index(ctx context.Context, force bool) (agent.RegistryIndex, error) {
	idx, err := (&agent.RegistryClient{HTTP: g.http, URL: g.url,
		Dir: g.regDir, Now: time.Now}).Fetch(ctx, force)
	if err != nil {
		if !errors.Is(err, agent.ErrCacheWrite) {
			return agent.RegistryIndex{}, err
		}
		g.log.Warn("registry cache write failed",
			"code", string(agent.CodeRegistryCacheWrite), "err", err)
	}
	return idx, nil
}

// entry 是索引裡的條目 id，版本要是頁面確認過的 version：索引裡沒有是 agent.registry_entry_missing（AC17），
// 版本變了是 agent.registry_entry_changed（確認之後索引更新了；條目會執行遠端程式碼，只跑使用者看過的，AC10、AC21）。
func (g *Agents) entry(ctx context.Context, id, version string) (agent.RegistryEntry, error) {
	idx, err := g.index(ctx, false)
	if err != nil {
		return agent.RegistryEntry{}, err
	}
	for _, e := range idx.Agents {
		if e.ID != id {
			continue
		}
		if e.Version != version {
			return agent.RegistryEntry{}, &agent.Problem{Code: agent.CodeRegistryEntryChanged,
				Params: map[string]string{"id": id, "version": e.Version}}
		}
		return e, nil
	}
	return agent.RegistryEntry{}, &agent.Problem{Code: agent.CodeRegistryEntryMissing, Params: map[string]string{"id": id}}
}

// AddRegistry 加入 Registry 條目 id 的 version 版（AC10–AC18）：存下索引裡的原始條目，之後一律依它啟動。
func (g *Agents) AddRegistry(ctx context.Context, id, version string) error {
	e, err := g.entry(ctx, id, version)
	if err != nil {
		return err
	}
	return g.change(ctx, func(c agent.Config) (agent.Config, error) {
		return c.AddRegistry(g.builtins, e, g.now())
	})
}

// UpdateRegistry 把加入的 Registry 條目 id 換成索引裡的 version 版（AC21）。
func (g *Agents) UpdateRegistry(ctx context.Context, id, version string) error {
	e, err := g.entry(ctx, id, version)
	if err != nil {
		return err
	}
	return g.change(ctx, func(c agent.Config) (agent.Config, error) { return c.UpdateRegistry(e) })
}

// Remove 移除加入的 Registry 條目或自訂 agent（AC22–AC25），連同覆蓋路徑、Profile 與記下的模型；它們的密鑰
// 記成待清理，接著清理一次（AC37、PO19）。設定存好後才刪模型，仍在 write 下：同一個 ID 不會在這之間又被加回、
// 設了新的預設模型再被刪掉。刪不掉只記日誌（移除已經生效）。
func (g *Agents) Remove(ctx context.Context, id task.AgentID) error {
	err := g.save(ctx, func(c agent.Config) (agent.Config, error) { return c.Remove(g.builtins, id) }, true, func() {
		if g.forget == nil {
			return
		}
		if err := g.forget(ctx, id); err != nil {
			g.log.Warn("app: forgetting the models of a removed agent failed", "agent", id, "err", err)
		}
	})
	if err != nil {
		return err
	}
	g.sweep(ctx)
	return nil
}

// AddCustom 新增自訂 agent（AC30–AC35）。
func (g *Agents) AddCustom(ctx context.Context, in agent.CustomAgentInput) error {
	return g.putCustom(ctx, in, true)
}

// EditCustom 改寫自訂 agent（AC36）：slug 不能改。
func (g *Agents) EditCustom(ctx context.Context, in agent.CustomAgentInput) error {
	return g.putCustom(ctx, in, false)
}

// putCustom 新增或改寫自訂 agent（putSecrets）：檢查 slug、命令、新增或改寫的對象與環境變數。
func (g *Agents) putCustom(ctx context.Context, in agent.CustomAgentInput, create bool) error {
	check := func(c agent.Config) ([]task.EnvVar, error) {
		if _, err := c.PutCustom(customAgent(in, nil), create); err != nil {
			return nil, err
		}
		var old []task.EnvVar
		if !create {
			for _, x := range c.Custom {
				if x.Slug == in.Slug {
					old = x.Env
				}
			}
		}
		return old, agent.CheckEnv(in.Env, old)
	}
	key := func(name string) string { return agent.SecretKey(in.Slug, name, rand.Text()) }
	put := func(c agent.Config, env []task.EnvVar) (agent.Config, error) {
		return c.PutCustom(customAgent(in, env), create)
	}
	return g.putSecrets(ctx, in.Env, key, check, put, true)
}

func customAgent(in agent.CustomAgentInput, env []task.EnvVar) agent.CustomAgent {
	return agent.CustomAgent{Slug: in.Slug, Name: in.Name, Command: strings.TrimSpace(in.Command), Args: in.Args, Env: env}
}

// AddProfile 給目錄裡的 agent 新建 Profile（PO03、PO31），回傳它的 ID。
func (g *Agents) AddProfile(ctx context.Context, in agent.ProfileInput) (task.ProfileID, error) {
	in.ID = agent.NewProfileID()
	if err := g.putProfile(ctx, in, true); err != nil {
		return "", err
	}
	return in.ID, nil
}

// EditProfile 改寫 agent 的 Profile（PO02、PO32），預設的也可以。
func (g *Agents) EditProfile(ctx context.Context, in agent.ProfileInput) error {
	return g.putProfile(ctx, in, false)
}

// putProfile 新增或改寫 Profile（putSecrets）：agent 要在目錄裡（agent.unknown_agent），其餘由
// Config.PutProfile 與 agent.CheckEnv 檢查。每次檢查都取當時目錄裡的 Definition（寫鑰匙串時目錄可能換了）。
// 只改設定、不換目錄（Profile 不影響發現）。
func (g *Agents) putProfile(ctx context.Context, in agent.ProfileInput, create bool) error {
	profile := func(env []task.EnvVar) agent.Profile {
		return agent.Profile{ID: in.ID, Name: in.Name, Approval: in.Approval, ExtraArgs: in.ExtraArgs, Env: env,
			AskTimeout: in.AskTimeout, TimeoutVerdict: in.TimeoutVerdict}
	}
	check := func(c agent.Config) ([]task.EnvVar, error) {
		def, ok := g.definition(in.Agent)
		if !ok {
			return nil, agent.ErrUnknownAgent
		}
		if _, err := c.PutProfile(def, profile(nil), create); err != nil {
			return nil, err
		}
		var old []task.EnvVar
		if !create {
			p, err := c.Profile(def.ID, in.ID)
			if err != nil {
				return nil, err
			}
			old = p.Env
		}
		return old, agent.CheckEnv(in.Env, old)
	}
	key := func(name string) string { return agent.ProfileSecretKey(in.Agent, in.ID, name, rand.Text()) }
	put := func(c agent.Config, env []task.EnvVar) (agent.Config, error) {
		def, ok := g.definition(in.Agent) // putSecrets 先 check 過；write 下目錄不會再變
		if !ok {
			return c, agent.ErrUnknownAgent
		}
		return c.PutProfile(def, profile(env), create)
	}
	return g.putSecrets(ctx, in.Env, key, check, put, false)
}

// RemoveProfile 刪掉 agent 的 Profile（PO04、PO05），它的密鑰記成待清理，接著清理一次（AC37）。
func (g *Agents) RemoveProfile(ctx context.Context, id task.AgentID, pid task.ProfileID) error {
	err := g.save(ctx, func(c agent.Config) (agent.Config, error) { return c.RemoveProfile(id, pid) }, false, nil)
	if err != nil {
		return err
	}
	g.sweep(ctx)
	return nil
}

// definition 是目錄裡的 agent id。
func (g *Agents) definition(id task.AgentID) (agent.Definition, bool) {
	defs := g.Definitions()
	i := slices.IndexFunc(defs, func(d agent.Definition) bool { return d.ID == id })
	if i < 0 {
		return agent.Definition{}, false
	}
	return defs[i], true
}

// putSecrets 是帶密鑰環境變數的設定改動（自訂 agent、Profile）：先依目前的設定 check（回傳改寫前的環境變數），
// 都對了才把新的密鑰寫進鑰匙串（每次一個新鍵，key 給），再存 put 算出的設定；存不進去就刪掉剛寫的。寫鑰匙串
// 可能要等系統詢問，所以在 write 之外寫，存設定時在 write 下重新 check 一次（設定可能已被別人改了），沿用的鍵
// 也要還在。不再用到的舊密鑰由 put 記成待清理，存好後清理一次（AC37）。所以設定裡的鍵總是指向寫好的值；
// 輸入錯了不碰鑰匙串，失敗也不留孤兒。recatalog 同 save。
func (g *Agents) putSecrets(ctx context.Context, in []agent.EnvInput, key func(name string) string,
	check func(agent.Config) ([]task.EnvVar, error), put func(agent.Config, []task.EnvVar) (agent.Config, error),
	recatalog bool) error {
	g.mu.Lock()
	cur := g.cfg
	g.mu.Unlock()
	old, err := check(cur)
	if err != nil {
		return err
	}
	env, written, err := g.writeEnv(in, old, key)
	if err != nil {
		g.deleteKeys(written)
		return err
	}
	err = g.save(ctx, func(c agent.Config) (agent.Config, error) {
		now, err := check(c)
		if err != nil {
			return c, err
		}
		for _, v := range env {
			if v.Secret != "" && !slices.Contains(written, v.Secret) &&
				!slices.ContainsFunc(now, func(o task.EnvVar) bool { return o.Secret == v.Secret }) {
				return c, &agent.Problem{Code: agent.CodeSecretEmpty, Params: map[string]string{"name": v.Name}}
			}
		}
		return put(c, env)
	}, recatalog, nil)
	if err != nil {
		g.deleteKeys(written)
		return err
	}
	g.sweep(ctx)
	return nil
}

// writeEnv 把檢查過的 in 變成設定裡的環境變數：新的密鑰以 key 給的新鍵寫進鑰匙串，沿用的用 old 裡原來那一行
// （Kept）的鍵、這一行現在的名稱。
// written 是這次寫的鍵（出錯時也回，呼叫者刪掉）。
func (g *Agents) writeEnv(in []agent.EnvInput, old []task.EnvVar, key func(name string) string) (env []task.EnvVar, written []string, err error) {
	for _, v := range in {
		name := strings.TrimSpace(v.Name)
		switch {
		case !v.Secret:
			env = append(env, task.EnvVar{Name: name, Value: v.Value})
		case v.Value == "":
			env = append(env, task.EnvVar{Name: name, Secret: old[agent.KeptSecret(old, v.Kept)].Secret})
		default:
			k := key(name)
			if err := g.keychain.Set(k, []byte(v.Value)); err != nil {
				return nil, written, &agent.Problem{Code: agent.CodeKeychainUnavailable, Params: map[string]string{"detail": err.Error()}}
			}
			written = append(written, k)
			env = append(env, task.EnvVar{Name: name, Secret: k})
		}
	}
	return env, written, nil
}

// sweep 清理待清理的密鑰（m4.md AC37–AC39b）：還被解析中的快照（leased）或 Task 的 Session 快照（refs）
// 引用的留著；其餘從鑰匙串刪掉，再移出待清理。先看 leased 再讀快照：沒登記的解析要嘛已寫進事件流（讀得到），
// 要嘛讀的是已換掉的目錄（不會用待清理的鍵）。刪不掉、讀不了快照、存不進設定都只記日誌，留給下次
// （編輯、移除已經成功）。
func (g *Agents) sweep(ctx context.Context) {
	if g.refs == nil || g.leased == nil {
		return
	}
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	if g.swept != nil {
		defer g.swept()
	}
	g.mu.Lock()
	retired := slices.Clone(g.cfg.Retired)
	g.mu.Unlock()
	var free []agent.RetiredSecret
	for _, r := range retired {
		if !g.leased(r.Key) {
			free = append(free, r)
		}
	}
	refs := map[task.AgentID]map[string]bool{} // 讀不了的 agent 是 nil：當它的鍵都還在用
	var gone []string
	for _, r := range free {
		used, ok := refs[r.Agent]
		if !ok {
			var err error
			if used, err = g.refs(ctx, r.Agent); err != nil {
				g.log.Warn("app: reading the secrets tasks still use failed; keeping them", "agent", r.Agent, "err", err)
				used = nil
			}
			refs[r.Agent] = used
		}
		if used == nil || used[r.Key] {
			continue // 已刪的照樣在下面移出待清理
		}
		if err := g.keychain.Delete(r.Key); err != nil && !errors.Is(err, provider.ErrNoSecret) {
			g.log.Warn("app: deleting a retired secret failed; will retry", "key", r.Key, "err", err)
			continue
		}
		gone = append(gone, r.Key)
	}
	if len(gone) == 0 {
		return
	}
	// 只改待清理：目錄不變，不必重組、不交給 Discovery。
	if err := g.save(ctx, func(c agent.Config) (agent.Config, error) { return c.Swept(gone), nil }, false, nil); err != nil {
		g.log.Warn("app: saving the swept secrets failed; will delete them again", "err", err)
	}
}

// retiredAny 回報 keys 裡有沒有待清理的密鑰。
func (g *Agents) retiredAny(keys []string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.ContainsFunc(g.cfg.Retired, func(r agent.RetiredSecret) bool { return slices.Contains(keys, r.Key) })
}

func (g *Agents) deleteKeys(keys []string) {
	for _, k := range keys {
		if err := g.keychain.Delete(k); err != nil {
			g.log.Warn("app: deleting a custom agent's secret failed", "key", k, "err", err)
		}
	}
}

// SetOverride 設或清掉（path 為 ""）目錄裡 agent 的覆蓋路徑（AC40–AC43）。
func (g *Agents) SetOverride(ctx context.Context, id task.AgentID, path string) error {
	return g.change(ctx, func(c agent.Config) (agent.Config, error) {
		if !slices.ContainsFunc(g.Definitions(), func(d agent.Definition) bool { return d.ID == id }) {
			return c, agent.ErrUnknownAgent
		}
		return c.SetOverride(id, path)
	})
}

// change 改目錄的設定：save，再換掉目錄、交給 Discovery。
func (g *Agents) change(ctx context.Context, edit func(agent.Config) (agent.Config, error)) error {
	return g.save(ctx, edit, true, nil)
}

// save 在 write 下以 edit 算出新設定、存好（AC07：存不進去時什麼都不變）。recatalog 時再換掉目錄、交給
// Discovery（apply）；否則只換設定（Profile、待清理的密鑰不影響目錄）。committed 非 nil 時在換好之後、仍在
// write 下呼叫。
func (g *Agents) save(ctx context.Context, edit func(agent.Config) (agent.Config, error), recatalog bool, committed func()) error {
	g.write.Lock()
	defer g.write.Unlock()
	g.mu.Lock()
	cur := g.cfg
	g.mu.Unlock()
	next, err := edit(cur)
	if err != nil {
		return err
	}
	if err := g.store.PutAgentConfig(ctx, next); err != nil {
		return err
	}
	if recatalog {
		g.apply(next)
	} else {
		g.mu.Lock()
		g.cfg = next
		g.mu.Unlock()
	}
	if committed != nil {
		committed()
	}
	return nil
}

// relay 是 saveInstallations 轉交的發現結果：out 同 Discovery.Changes 只留最新的一份，in 關閉後
// 關閉 out 與 done。
type relay struct {
	out  chan []agent.Installation
	done chan struct{}
}

// saveInstallations 把 Discovery 的每一份表存進 store（下次啟動時的 Previous，AC05），再交給 out 的
// 讀取方（廣播 agents:changed）。存不進去只記日誌：這是啟動時先顯示的快取。唯一讀 in 的 goroutine。
func saveInstallations(in <-chan []agent.Installation, store agent.ConfigStore, log *slog.Logger) *relay {
	r := &relay{out: make(chan []agent.Installation, 1), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		defer close(r.out)
		for t := range in {
			if err := store.PutLastInstallations(context.Background(), t); err != nil {
				log.Warn("app: saving the discovery results failed", "err", err)
			}
			select { // 讀得慢就丟掉沒讀的舊表，只留最新的（同 Discovery.Changes）
			case <-r.out:
			default:
			}
			r.out <- t
		}
	}()
	return r
}
