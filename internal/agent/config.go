package agent

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// Config 是使用者對 Agent 目錄的設定（m4.md 第 2 節，D59）：加入的 Registry 條目（依加入時間）、
// 自訂 agent（依 slug）、覆蓋路徑（依 Agent ID）、Profile（m4.md 第 3 節）與待清理的密鑰。存在 store 的設定表，
// 不進事件日誌。
type Config struct {
	Registry  []RegistryAgent `json:"registry"`
	Custom    []CustomAgent   `json:"custom"`
	Overrides []Override      `json:"overrides"`
	// Profiles 是使用者建的 Profile 與改過的預設 Profile（依建立的順序）；沒改過的預設 Profile 不存（PO01）。
	Profiles []Profile `json:"profiles"`
	// Retired 是換下來、還沒從鑰匙串刪掉的密鑰（m4.md 規則 7、AC37–AC39b）：Task 的 Session 快照
	// 還引用它時不能刪，等沒有引用了由 app 清理。依換下來的順序。
	Retired []RetiredSecret `json:"retired"`
}

// RetiredSecret 是待清理的密鑰：Key 是鑰匙串裡的鍵，Agent 是用過它的 agent（清理時只看用過它的 Task）。
type RetiredSecret struct {
	Agent task.AgentID `json:"agent"`
	Key   string       `json:"key"`
}

// ConfigStore 保存 Config 與上一次的發現結果（store 實作；app 使用）。
type ConfigStore interface {
	// AgentConfig 回傳存下的設定；沒存過時是零值。
	AgentConfig(ctx context.Context) (Config, error)
	PutAgentConfig(ctx context.Context, c Config) error
	// LastInstallations 是上一次的發現結果（DiscoveryOptions.Previous，AC05）；沒存過時是 nil。
	LastInstallations(ctx context.Context) ([]Installation, error)
	PutLastInstallations(ctx context.Context, t []Installation) error
}

// CustomAgentInput 是頁面送來的自訂 agent（AC30–AC36）。密鑰的值只在這裡經過，存進鑰匙串，不回到頁面。
type CustomAgentInput struct {
	Slug    string     `json:"slug"`
	Name    string     `json:"name"`
	Command string     `json:"command"`
	Args    []string   `json:"args"`
	Env     []EnvInput `json:"env"`
}

// EnvInput 是頁面送來的一個環境變數（自訂 agent、Profile）。Secret 為真時 Value 存進鑰匙串。改寫時 Kept 非空
// 表示這一行是原來名為 Kept 的密鑰（頁面上的那一行，名稱可能改了），Value 為空時沿用它的值；Kept 為空時空值是
// agent.secret_empty：刪掉一行又加回同名的不算沿用（AC35、PO13）。
type EnvInput struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
	Kept   string `json:"kept"`
}

// Clone 是不與 c 共用切片的複本。
func (c Config) Clone() Config {
	return Config{Registry: slices.Clone(c.Registry), Custom: slices.Clone(c.Custom), Overrides: slices.Clone(c.Overrides),
		Profiles: slices.Clone(c.Profiles), Retired: slices.Clone(c.Retired)}
}

// AddRegistry 加入索引裡的條目 e（AC10、AC15、AC16）：已加入過是 agent.already_added；
// 已由 builtin 原生支援（Supersedes）是 agent.duplicate_agent；沒有本平台可用的發行方式是
// agent.not_runnable。builtins 是內建的 Definition。
func (c Config) AddRegistry(builtins []Definition, e RegistryEntry, now time.Time) (Config, error) {
	if slices.ContainsFunc(c.Registry, func(a RegistryAgent) bool { return a.ID == e.ID }) {
		return c, fail(CodeAlreadyAdded, nil, "id", e.ID)
	}
	if superseded(builtins, e.ID) {
		return c, fail(CodeDuplicateAgent, nil)
	}
	a, err := AddRegistry(e, now)
	if err != nil {
		return c, err
	}
	next := c.Clone()
	next.Registry = append(next.Registry, a)
	return next, nil
}

// UpdateRegistry 以索引裡的新條目 e 換掉已加入的同 ID 條目（AC21）：ID 與加入時間不變，
// 因此目錄裡的位置與 Profile 都留著。沒加入過是 agent.unknown_agent。
func (c Config) UpdateRegistry(e RegistryEntry) (Config, error) {
	i := slices.IndexFunc(c.Registry, func(a RegistryAgent) bool { return a.ID == e.ID })
	if i < 0 {
		return c, ErrUnknownAgent
	}
	a, err := AddRegistry(e, c.Registry[i].AddedAt)
	if err != nil {
		return c, err
	}
	next := c.Clone()
	next.Registry[i] = a
	return next, nil
}

// Remove 移除加入的 Registry 條目或自訂 agent，連同它的覆蓋路徑與 Profile（AC22、PO19）：內建的是 agent.builtin
// （AC25），不在設定裡是 agent.unknown_agent。也移除得了解析不了、沒進目錄的（AC03）。
// 自訂 agent 與它的 Profile 的密鑰記成待清理（AC35、AC37）。
func (c Config) Remove(builtins []Definition, id task.AgentID) (Config, error) {
	if slices.ContainsFunc(builtins, func(d Definition) bool { return d.ID == id }) {
		return c, fail(CodeBuiltin, nil)
	}
	next := c.Clone()
	switch {
	case strings.HasPrefix(string(id), "registry:"):
		n := len(next.Registry)
		next.Registry = slices.DeleteFunc(next.Registry, func(a RegistryAgent) bool { return registryID(a.ID) == id })
		if len(next.Registry) == n {
			return c, ErrUnknownAgent
		}
	case strings.HasPrefix(string(id), "custom:"):
		i := slices.IndexFunc(next.Custom, func(a CustomAgent) bool { return customID(a.Slug) == id })
		if i < 0 {
			return c, ErrUnknownAgent
		}
		next.retire(id, next.Custom[i].Env, nil)
		next.Custom = slices.Delete(next.Custom, i, i+1)
	default:
		return c, ErrUnknownAgent
	}
	next.Overrides = slices.DeleteFunc(next.Overrides, func(o Override) bool { return o.Agent == id })
	next.Profiles = slices.DeleteFunc(next.Profiles, func(p Profile) bool {
		if p.Agent == id {
			next.retire(id, p.Env, nil)
		}
		return p.Agent == id
	})
	return next, nil
}

// PutCustom 新增（create）或改寫自訂 agent a（AC30–AC36）：先依 CustomAgent.Definition 檢查；
// 新增時 slug 已被用了是 agent.duplicate_agent，改寫時沒有這個 slug 是 agent.unknown_agent。
// 名稱為空時用 slug（AC34）。改寫時原來有、a 不再用的密鑰記成待清理（AC37）。自訂 agent 依 slug 排序。
func (c Config) PutCustom(a CustomAgent, create bool) (Config, error) {
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		a.Name = a.Slug
	}
	if _, err := a.Definition(); err != nil {
		return c, err
	}
	i := slices.IndexFunc(c.Custom, func(x CustomAgent) bool { return x.Slug == a.Slug })
	switch {
	case create && i >= 0:
		return c, fail(CodeDuplicateAgent, nil)
	case !create && i < 0:
		return c, ErrUnknownAgent
	}
	next := c.Clone()
	if create {
		next.Custom = append(next.Custom, a)
	} else {
		next.retire(customID(a.Slug), next.Custom[i].Env, a.Env)
		next.Custom[i] = a
	}
	slices.SortFunc(next.Custom, func(x, y CustomAgent) int { return strings.Compare(x.Slug, y.Slug) })
	return next, nil
}

// SetOverride 設或清掉（path 為 ""）agent 的覆蓋路徑（AC40–AC42）：path 去掉首尾空白，
// 不是絕對路徑是 agent.relative_override。agent 是否在目錄裡由呼叫者確認。覆蓋路徑依 Agent ID 排序。
func (c Config) SetOverride(id task.AgentID, path string) (Config, error) {
	path = strings.TrimSpace(path)
	if path != "" && !filepath.IsAbs(path) {
		return c, fail(CodeRelativeOverride, nil, "path", path)
	}
	next := c.Clone()
	next.Overrides = slices.DeleteFunc(next.Overrides, func(o Override) bool { return o.Agent == id })
	if path != "" {
		next.Overrides = append(next.Overrides, Override{Agent: id, Executable: path})
		slices.SortFunc(next.Overrides, func(x, y Override) int { return strings.Compare(string(x.Agent), string(y.Agent)) })
	}
	return next, nil
}

// retire 把 env 裡不在 keep 中的密鑰記成 agent 的待清理密鑰。只在 Clone 出來的設定上呼叫。
func (c *Config) retire(agent task.AgentID, env, keep []task.EnvVar) {
	for _, v := range env {
		if v.Secret != "" && !slices.ContainsFunc(keep, func(k task.EnvVar) bool { return k.Secret == v.Secret }) {
			c.Retired = append(c.Retired, RetiredSecret{Agent: agent, Key: v.Secret})
		}
	}
}

// Swept 移出已從鑰匙串刪掉的密鑰 keys（AC37、AC39）。
func (c Config) Swept(keys []string) Config {
	next := c.Clone()
	next.Retired = slices.DeleteFunc(next.Retired, func(r RetiredSecret) bool { return slices.Contains(keys, r.Key) })
	return next
}

// Override 回傳 agent 的覆蓋路徑；沒設時是 ""。
func (c Config) Override(id task.AgentID) string {
	for _, o := range c.Overrides {
		if o.Agent == id {
			return o.Executable
		}
	}
	return ""
}

// superseded 回報 Registry 條目 id 是否已由某個 builtin 原生支援。
func superseded(builtins []Definition, id string) bool {
	return slices.ContainsFunc(builtins, func(d Definition) bool { return slices.Contains(d.Supersedes, id) })
}

// SecretKey 是自訂 agent 的密鑰環境變數在鑰匙串裡的鍵（AC35）。nonce 讓每次寫入用新的鍵：
// 設定存好之前舊鍵的值不被蓋掉，存不進去時刪掉新鍵即可。
func SecretKey(slug, name, nonce string) string {
	return "agent:custom:" + slug + ":" + name + ":" + nonce
}

// ProfileSecretKey 是 Profile 的密鑰環境變數在鑰匙串裡的鍵（PO13）；nonce 同 SecretKey。
func ProfileSecretKey(agent task.AgentID, profile task.ProfileID, name, nonce string) string {
	return "profile:" + string(agent) + ":" + string(profile) + ":" + name + ":" + nonce
}

// ProfilesOf 是 agent id 的 Profile：預設的在最前（沒改過時是 DefaultProfile），其餘依建立的順序（PO01、PO30）。
// 是複本，不與設定共用切片。
func (c Config) ProfilesOf(id task.AgentID) []Profile {
	out := []Profile{defaultProfile(id)}
	for _, p := range c.Profiles {
		if p.Agent != id {
			continue
		}
		p.ExtraArgs, p.Env = slices.Clone(p.ExtraArgs), slices.Clone(p.Env)
		if p.Default {
			out[0] = p
		} else {
			out = append(out, p)
		}
	}
	return out
}

// Profile 是 agent id 的 Profile pid；pid 為空時是預設的（PO18）。沒有時是 agent.unknown_profile（PO17）。
func (c Config) Profile(id task.AgentID, pid task.ProfileID) (Profile, error) {
	if pid == "" {
		pid = DefaultProfileID
	}
	for _, p := range c.ProfilesOf(id) {
		if p.ID == pid {
			return p, nil
		}
	}
	return Profile{}, ErrUnknownProfile
}

// NewProfileID 是新建 Profile 的 ID：p_ 加隨機串（PO03）。
func NewProfileID() task.ProfileID {
	return task.ProfileID("p_" + strings.ToLower(rand.Text()[:12]))
}

// PutProfile 新增（create）或改寫 def 的 Profile p（PO03–PO14），環境變數由呼叫者以 CheckEnv 檢查過：
//   - 新增時 p.ID 由呼叫者給（NewProfileID），改寫時 agent 沒有這個 ID 是 agent.unknown_profile；
//   - 名稱去掉首尾空白，不能為空（agent.profile_name_empty），在這個 agent 裡不能重複（不分大小寫，
//     agent.profile_name_taken）；預設 Profile 不能改名，名稱照舊；
//   - 審批空著是經 OrielHub；選 agent 自己的寬鬆模式而 def 沒有對應參數是 agent.native_unsupported（PO07）；
//   - 超時在 0 到 24 小時之間（agent.invalid_timeout，PO10），為正時要有超時後的決定（agent.timeout_verdict_missing，
//     PO09），為 0 時清掉它。
//
// 改寫時原來有、p 不再用的密鑰記成待清理（AC37）。
func (c Config) PutProfile(def Definition, p Profile, create bool) (Config, error) {
	p.Agent = def.ID
	mine := c.ProfilesOf(def.ID)
	i := slices.IndexFunc(mine, func(x Profile) bool { return x.ID == p.ID })
	switch {
	case create && (i >= 0 || p.ID == ""):
		return c, fail(CodeInternal, nil, "detail", "profile ID "+string(p.ID)+" is taken")
	case !create && i < 0:
		return c, ErrUnknownProfile
	}
	p.Default = p.ID == DefaultProfileID
	if p.Default {
		p.Name = mine[0].Name
	} else {
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			return c, fail(CodeProfileNameEmpty, nil)
		}
		if slices.ContainsFunc(mine, func(x Profile) bool { return x.ID != p.ID && strings.EqualFold(x.Name, p.Name) }) {
			return c, fail(CodeProfileNameTaken, nil, "name", p.Name)
		}
	}
	switch p.Approval {
	case "", task.ApprovalOrielHub:
		p.Approval = task.ApprovalOrielHub
	case task.ApprovalNative:
		if def.Launch.Native == nil {
			return c, fail(CodeNativeUnsupported, nil)
		}
	default:
		return c, fail(CodeInternal, nil, "detail", "unknown approval mode "+string(p.Approval))
	}
	switch {
	case p.AskTimeout < 0 || p.AskTimeout > MaxAskTimeout:
		return c, fail(CodeInvalidTimeout, nil)
	case p.AskTimeout == 0:
		p.TimeoutVerdict = ""
	case p.TimeoutVerdict != task.Allow && p.TimeoutVerdict != task.Deny:
		return c, fail(CodeTimeoutVerdictMissing, nil)
	}
	next := c.Clone()
	j := slices.IndexFunc(next.Profiles, func(x Profile) bool { return x.Agent == def.ID && x.ID == p.ID })
	if j >= 0 {
		next.retire(def.ID, next.Profiles[j].Env, p.Env)
		next.Profiles[j] = p
	} else {
		next.Profiles = append(next.Profiles, p)
	}
	return next, nil
}

// RemoveProfile 刪掉 agent id 的 Profile pid（PO04、PO05）：預設的是 agent.default_profile，沒有是
// agent.unknown_profile。它的密鑰記成待清理（AC37）；已有 Session 用自己的快照，不受影響。
func (c Config) RemoveProfile(id task.AgentID, pid task.ProfileID) (Config, error) {
	if pid == DefaultProfileID {
		return c, fail(CodeDefaultProfile, nil)
	}
	j := slices.IndexFunc(c.Profiles, func(x Profile) bool { return x.Agent == id && x.ID == pid })
	if j < 0 {
		return c, ErrUnknownProfile
	}
	next := c.Clone()
	next.retire(id, next.Profiles[j].Env, nil)
	next.Profiles = slices.Delete(next.Profiles, j, j+1)
	return next, nil
}

// CheckEnv 檢查頁面送來的環境變數 in（AC35、PO11、PO13）：名稱去掉首尾空白後非空、不含 =、不重複
// （agent.invalid_env）；密鑰沒給值時要有 Kept，且 old（改寫前的環境變數）裡有名為 Kept 的密鑰，否則是
// agent.secret_empty。按原來的名稱找，所以改了名的行沿用的是它自己原來的值。
func CheckEnv(in []EnvInput, old []task.EnvVar) error {
	seen := map[string]bool{}
	for _, v := range in {
		name := strings.TrimSpace(v.Name)
		if name == "" || strings.Contains(name, "=") || seen[name] {
			return fail(CodeInvalidEnv, nil, "name", name)
		}
		seen[name] = true
		if v.Secret && v.Value == "" && (v.Kept == "" || KeptSecret(old, v.Kept) < 0) {
			return fail(CodeSecretEmpty, nil, "name", name)
		}
	}
	return nil
}

// KeptSecret 是 old 裡名為 name 的密鑰的位置；沒有時是 -1。
func KeptSecret(old []task.EnvVar, name string) int {
	return slices.IndexFunc(old, func(o task.EnvVar) bool { return o.Name == name && o.Secret != "" })
}
