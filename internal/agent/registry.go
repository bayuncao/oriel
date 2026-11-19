package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// RegistryURL 是 ACP Registry 發佈的索引。
const RegistryURL = "https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json"

// registryTTL 是已擷取的索引在重新驗證前的使用時長。
const registryTTL = 24 * time.Hour

// RegistryEntry 是 ACP Registry registry.json 中的一個 agent
// （agentclientprotocol/registry FORMAT.md）。未知欄位會被忽略；
// 不使用 preview channel。
type RegistryEntry struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Description  string       `json:"description,omitzero"`
	Repository   string       `json:"repository,omitzero"`
	Website      string       `json:"website,omitzero"`
	Authors      []string     `json:"authors,omitzero"`
	License      string       `json:"license,omitzero"`
	Icon         string       `json:"icon,omitzero"`
	Distribution Distribution `json:"distribution"`

	raw json.RawMessage // registry.json 中的原始條目，連同未知欄位
}

// Distribution 說明條目的執行方式。OrielHub 透過使用者的
// node 與 uv 執行 npx 和 uvx 條目；對 binary 條目只
// 顯示取得位置，並以名稱偵測其命令。
type Distribution struct {
	Binary map[string]BinaryTarget `json:"binary,omitzero"` // 依平台區分，例如 "darwin-aarch64"
	NPX    *PackageTarget          `json:"npx,omitzero"`
	UVX    *PackageTarget          `json:"uvx,omitzero"`
}

type BinaryTarget struct {
	Archive string            `json:"archive"`
	SHA256  string            `json:"sha256,omitzero"`
	Cmd     string            `json:"cmd"`
	Args    []string          `json:"args,omitzero"`
	Env     map[string]string `json:"env,omitzero"`
}

type PackageTarget struct {
	Package string            `json:"package"` // 通常已釘選："pkg@1.2.3"、"pkg==1.2.3"
	Args    []string          `json:"args,omitzero"`
	Env     map[string]string `json:"env,omitzero"`
}

// RegistryIndex 是已擷取的 registry.json。
type RegistryIndex struct {
	Version   string          `json:"version"`
	Agents    []RegistryEntry `json:"agents"`
	FetchedAt time.Time       `json:"-"` // 伺服器最後確認它的時間
	// Stale 為零表示這次確認成功或使用新鮮快取；非零表示這次確認
	// 失敗、這是快取，值是失敗的原因。
	Stale Problem `json:"-"`
}

// RegistryClient 擷取索引並快取在 Dir 中，每天最多以其
// ETag 重新驗證一次。離線時提供快取。
type RegistryClient struct {
	HTTP *http.Client // 必填；app 提供一個使用使用者代理的 client
	URL  string       // "" 表示 RegistryURL
	Dir  string       // 快取目錄，位於 app 的資料目錄中
	Now  func() time.Time
}

// registryMeta 保存在快取的索引旁邊。
type registryMeta struct {
	ETag      string    `json:"etag"`
	FetchedAt time.Time `json:"fetchedAt"`
}

// Fetch 回傳索引：仍新鮮時回傳快取，除非 force 為真，
// 否則回傳重新驗證或新下載的索引。下載失敗時回傳
// 快取並以 Stale 記下失敗的原因，若無快取則回傳錯誤。錯誤為
// ErrCacheWrite 時索引仍可用，其他錯誤時索引為空。
func (c *RegistryClient) Fetch(ctx context.Context, force bool) (RegistryIndex, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	cached, meta, cacheErr := c.load()
	if cacheErr == nil && !force && now().Sub(meta.FetchedAt) < registryTTL {
		return cached, nil
	}
	idx, etag, err := c.download(ctx, meta.ETag, cacheErr == nil)
	switch {
	case err != nil && cacheErr == nil:
		var p *Problem
		if errors.As(err, &p) {
			cached.Stale = *p
		} else {
			cached.Stale = problem(CodeRegistryUnreachable, "detail", err.Error())
		}
		return cached, nil
	case err != nil:
		return RegistryIndex{}, err
	case idx == nil: // 未修改
		meta.FetchedAt = now()
		cached.FetchedAt = meta.FetchedAt
		return cached, c.saveMeta(meta)
	}
	meta = registryMeta{ETag: etag, FetchedAt: now()}
	idx.index.FetchedAt = meta.FetchedAt
	if err := writeAtomic(filepath.Join(c.Dir, "registry.json"), idx.raw); err != nil {
		return idx.index, fail(CodeRegistryCacheWrite, err, "detail", err.Error())
	}
	return idx.index, c.saveMeta(meta)
}

type downloaded struct {
	index RegistryIndex
	raw   []byte
}

// download 在伺服器回答 304 時回傳 nil 且無錯誤。
func (c *RegistryClient) download(ctx context.Context, etag string, haveCache bool) (*downloaded, string, error) {
	if c.HTTP == nil {
		return nil, "", fail(CodeRegistryNoClient, nil)
	}
	url := cmp.Or(c.URL, RegistryURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fail(CodeRegistryUnreachable, err, "detail", err.Error())
	}
	if etag != "" && haveCache {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", fail(CodeRegistryUnreachable, err, "detail", err.Error())
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && haveCache:
		return nil, etag, nil
	case resp.StatusCode != http.StatusOK:
		return nil, "", fail(CodeRegistryStatus, nil, "status", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", fail(CodeRegistryUnreachable, err, "detail", err.Error())
	}
	idx, err := ParseRegistry(raw)
	if err != nil {
		return nil, "", err
	}
	return &downloaded{index: idx, raw: raw}, resp.Header.Get("ETag"), nil
}

func (c *RegistryClient) load() (RegistryIndex, registryMeta, error) {
	var meta registryMeta
	raw, err := os.ReadFile(filepath.Join(c.Dir, "registry.json"))
	if err != nil {
		return RegistryIndex{}, meta, err
	}
	idx, err := ParseRegistry(raw)
	if err != nil {
		return RegistryIndex{}, meta, err
	}
	if b, err := os.ReadFile(filepath.Join(c.Dir, "registry.meta.json")); err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	idx.FetchedAt = meta.FetchedAt
	return idx, meta, nil
}

func (c *RegistryClient) saveMeta(m registryMeta) error {
	b, _ := json.Marshal(m)
	if err := writeAtomic(filepath.Join(c.Dir, "registry.meta.json"), b); err != nil {
		return fail(CodeRegistryCacheWrite, err, "detail", err.Error())
	}
	return nil
}

// writeAtomic 透過暫存檔將 data 取代到 path，因此
// 當機絕不會留下寫到一半的快取。
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// ParseRegistry 解碼 registry.json，並為每個條目留下原始位元組。
func ParseRegistry(raw []byte) (RegistryIndex, error) {
	var doc struct {
		Version string            `json:"version"`
		Agents  []json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return RegistryIndex{}, fail(CodeRegistryInvalid, err, "detail", err.Error())
	}
	idx := RegistryIndex{Version: doc.Version}
	for _, r := range doc.Agents {
		var e RegistryEntry
		if err := json.Unmarshal(r, &e); err != nil {
			return RegistryIndex{}, fail(CodeRegistryInvalid, err, "detail", err.Error())
		}
		if e.ID != "" {
			e.raw = r
			idx.Agents = append(idx.Agents, e)
		}
	}
	return idx, nil
}

// RegistryListing 是提供給使用者加入的 Registry 條目。
type RegistryListing struct {
	Entry RegistryEntry `json:"entry"`
	// Native 是已以其原生協議支援此條目的 builtin Agent；
	// 這類條目預設隱藏。
	Native task.AgentID `json:"native,omitzero"`
	// Added 是使用者已加入此條目時的釘選版本。
	Added string `json:"added,omitzero"`
	// Update 表示 Registry 有與釘選版本不同的版本；
	// 升級需要使用者確認。
	Update bool `json:"update"`
	// Runnable 在此平台既無 npx、uvx 也無 binary 時為 false。
	Runnable bool `json:"runnable"`
	// Installed 是本機已裝的對應程式（FindInstalled），可直接加入。
	Installed LocalInstall `json:"installed,omitzero"`
}

// RegistryList 是 Agent 頁看到的 Registry：條目加上索引的新舊。
type RegistryList struct {
	Listings  []RegistryListing `json:"listings"`
	FetchedAt time.Time         `json:"fetchedAt,omitzero"` // 伺服器最後確認索引的時間；沒有記錄時為零（頁面收不到此欄）
	Stale     Problem           `json:"stale,omitzero"`     // 非零：這次確認失敗，顯示的是快取；值為原因
}

// ListRegistry 將索引與 builtin、已加入的條目及本機已裝的程式配對。
func ListRegistry(idx RegistryIndex, builtins []Definition, added []RegistryAgent, installed map[string]LocalInstall) []RegistryListing {
	native := map[string]task.AgentID{}
	for _, b := range builtins {
		for _, id := range b.Supersedes {
			native[id] = b.ID
		}
	}
	pinned := map[string]string{}
	for _, a := range added {
		pinned[a.ID] = a.Version
	}
	out := make([]RegistryListing, 0, len(idx.Agents))
	for _, e := range idx.Agents {
		_, err := entryDefinition(e)
		out = append(out, RegistryListing{
			Entry:     e,
			Native:    native[e.ID],
			Added:     pinned[e.ID],
			Update:    pinned[e.ID] != "" && pinned[e.ID] != e.Version,
			Runnable:  err == nil,
			Installed: installed[e.ID],
		})
	}
	return out
}

// AddRegistry 在使用者加入 e 或確認升級時將其釘選，保存
// registry.json 中的原始條目（含 OrielHub 不認得的欄位）。檢查與
// 記錄的版本都取自保存的位元組，因此兩者不會不一致；不是由
// ParseRegistry 得到的條目則先序列化。
func AddRegistry(e RegistryEntry, now time.Time) (RegistryAgent, error) {
	raw := slices.Clone([]byte(e.raw))
	if raw == nil {
		var err error
		if raw, err = json.Marshal(e); err != nil {
			return RegistryAgent{}, err
		}
	}
	a := RegistryAgent{ID: e.ID, Entry: raw, AddedAt: now}
	saved, err := a.entry()
	if err != nil {
		return RegistryAgent{}, err
	}
	if _, err := entryDefinition(saved); err != nil {
		return RegistryAgent{}, err
	}
	a.Version = saved.Version
	return a, nil
}

// Definition 是目錄執行已加入條目時的形式，取自釘選的
// 條目而非目前的索引。
func (a RegistryAgent) Definition() (Definition, error) {
	e, err := a.entry()
	if err != nil {
		return Definition{}, err
	}
	return entryDefinition(e)
}

// entry 解碼保存的條目，並確認它就是 a 所指的那一個。
func (a RegistryAgent) entry() (RegistryEntry, error) {
	var e RegistryEntry
	if err := json.Unmarshal(a.Entry, &e); err != nil {
		return RegistryEntry{}, fail(CodeRegistryEntryInvalid, err, "id", a.ID, "detail", err.Error())
	}
	if e.ID != a.ID {
		return RegistryEntry{}, fail(CodeRegistryEntryMismatch, nil, "id", a.ID, "holds", e.ID)
	}
	return e, nil
}

// entryDefinition 優先使用 npx，其次 uvx，最後是此平台的
// binary。npx 與 uvx 條目偵測執行器並啟動釘選的套件；
// 其身分就是釘選的版本，因此沒有版本探測（ADR-25）。
func entryDefinition(e RegistryEntry) (Definition, error) {
	def := Definition{
		ID:       registryID(e.ID),
		Name:     e.Name,
		Source:   SourceRegistry,
		Protocol: task.ProtocolACP,
		Install:  InstallHint{DocsURL: cmp.Or(e.Website, e.Repository)},
	}
	c, err := e.Distribution.choose()
	if err != nil {
		return Definition{}, err
	}
	if r := c.runner; r != nil {
		// 缺的是執行器，因此安裝提示指向 node / uv，而不是條目的網站。
		args := append(slices.Clone(r.flags), r.pin(c.pkg.Package, e.Version))
		def.Detect = DetectSpec{Command: r.cmd}
		def.Launch = LaunchSpec{Args: append(args, c.pkg.Args...), Env: envVars(c.pkg.Env)}
		def.Install = InstallHint{DocsURL: r.docs}
		return def, nil
	}
	def.Detect = DetectSpec{Command: filepath.Base(c.bin.Cmd)}
	def.Launch = LaunchSpec{Args: slices.Clone(c.bin.Args), Env: envVars(c.bin.Env)}
	return def, nil
}

// choice 是條目選用的發行方式：套件條目（npx、uvx）有 runner 與
// pkg，binary 條目只有 bin。
type choice struct {
	runner *packageRunner
	pkg    *PackageTarget
	bin    BinaryTarget
}

func (c choice) kind() distKind {
	if c.runner == nil {
		return distBinary
	}
	return c.runner.kind
}

type distKind string

const (
	distNPX    distKind = "npx"
	distUVX    distKind = "uvx"
	distBinary distKind = "binary"
)

// choose 依 npx → uvx → 本平台 binary 的順序選出第一個可用的發行方式。
func (d Distribution) choose() (choice, error) {
	switch {
	case d.NPX != nil && d.NPX.Package != "":
		return choice{runner: &npx, pkg: d.NPX}, nil
	case d.UVX != nil && d.UVX.Package != "":
		return choice{runner: &uvx, pkg: d.UVX}, nil
	}
	p, ok := platform()
	if !ok {
		return choice{}, fail(CodeNotRunnable, nil, "platform", runtime.GOOS+"/"+runtime.GOARCH)
	}
	if b := d.Binary[p]; b.Cmd != "" {
		return choice{bin: b}, nil
	}
	return choice{}, fail(CodeNotRunnable, nil, "platform", p)
}

// packageRunner 是執行套件條目的程式：OrielHub 偵測它、以釘選的
// 套件啟動它，缺少時指向它的安裝說明。
type packageRunner struct {
	kind  distKind
	cmd   string
	flags []string
	sep   string // 套件名與版本之間的分隔："@"、"=="
	docs  string
}

var (
	npx = packageRunner{kind: distNPX, cmd: "npx", flags: []string{"-y"}, sep: "@", docs: "https://nodejs.org/en/download"}
	uvx = packageRunner{kind: distUVX, cmd: "uvx", sep: "==", docs: "https://docs.astral.sh/uv/getting-started/installation/"}
)

// split 把套件規格分成名稱與版本："@scope/name@1.2" → "@scope/name"、"1.2"。
// scope 開頭的 @ 不是版本分隔。
func (r *packageRunner) split(spec string) (name, version string) {
	scope := ""
	if r.sep == "@" && strings.HasPrefix(spec, "@") {
		scope, spec = "@", spec[1:]
	}
	if i := strings.LastIndex(spec, r.sep); i > 0 {
		return scope + spec[:i], spec[i+len(r.sep):]
	}
	return scope + spec, ""
}

// pin 為缺少版本的套件規格加上條目的版本。
func (r *packageRunner) pin(spec, version string) string {
	if _, v := r.split(spec); v != "" || version == "" {
		return spec
	}
	return spec + r.sep + version
}

func envVars(m map[string]string) []task.EnvVar {
	var out []task.EnvVar
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, task.EnvVar{Name: k, Value: m[k]})
	}
	return out
}

// platform 是此機器在 Distribution.Binary 中的鍵；Registry 沒有
// 對應的架構時 ok 為 false。
func platform() (key string, ok bool) {
	arch, ok := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	return runtime.GOOS + "-" + arch, ok
}
