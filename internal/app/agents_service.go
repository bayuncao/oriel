package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
)

// Agents 包著 Discovery 與 Registry 快取，供 transport 使用。
type Agents struct {
	disc   *agent.Discovery
	defs   []agent.Definition
	http   *http.Client
	regDir string
	url    string       // Registry 索引的位址；"" → agent.RegistryURL
	root   string       // 檔案系統根；僅測試
	log    *slog.Logger // nil → slog.Default()
}

func (g *Agents) Installations() []agent.Installation { return g.disc.Installations() }
func (g *Agents) Definitions() []agent.Definition     { return g.defs }
func (g *Agents) Refresh(ctx context.Context) error   { return g.disc.Refresh(ctx) }

// Registry 取 Registry 列表（唯讀；force 重新確認）。索引取得但
// 快取寫入失敗時仍回傳列表，只記下警告。
func (g *Agents) Registry(ctx context.Context, force bool) (agent.RegistryList, error) {
	idx, err := (&agent.RegistryClient{HTTP: g.http, URL: g.url,
		Dir: g.regDir, Now: time.Now}).Fetch(ctx, force)
	if err != nil {
		if !errors.Is(err, agent.ErrCacheWrite) {
			return agent.RegistryList{}, err
		}
		g.log.Warn("registry cache write failed",
			"code", string(agent.CodeRegistryCacheWrite), "err", err)
	}
	env, err := g.disc.Env(ctx)
	if err != nil {
		return agent.RegistryList{}, err
	}
	installed := agent.FindInstalled(env, g.root, idx)
	return agent.RegistryList{
		Listings:  agent.ListRegistry(idx, g.defs, nil, installed),
		FetchedAt: idx.FetchedAt,
		Stale:     idx.Stale,
	}, nil
}
