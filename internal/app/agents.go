package app

import (
	"log/slog"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/runtime/acp"
	"github.com/bayuncao/oriel/internal/runtime/claude"
	"github.com/bayuncao/oriel/internal/runtime/codex"
	"github.com/bayuncao/oriel/internal/runtime/pi"
	"github.com/bayuncao/oriel/internal/store"
	"github.com/bayuncao/oriel/internal/usage"
)

// builtins 是內建的 Agent，依目錄列出的順序明確註冊。
func builtins() []agent.Definition {
	return []agent.Definition{
		claude.Definition(), codex.Definition(), pi.Definition(),
		acp.Grok(), acp.Gemini(), acp.OpenCode(), acp.Cursor(), acp.Copilot(), acp.Qwen(),
	}
}

// usageSources 讀各內建 agent 在本機的用量記錄，依內建的順序（docs/architecture/
// usage.md）；OpenCode 的 SQLite 記錄經 store 的唯讀查詢讀（D90）。
func usageSources() []usage.Source {
	return []usage.Source{
		claude.Usage(), codex.Usage(), pi.Usage(),
		acp.GrokUsage(), acp.GeminiUsage(), acp.OpenCodeUsage(store.QueryFile), acp.CursorUsage(),
		acp.CopilotUsage(), acp.QwenUsage(),
	}
}

// catalog 是經 agent.Catalog 合併的目錄（標上 Source）；M1 只有 builtin。
// 每次呼叫都是新的一份，discovery 與 Agents 服務不共用切片。
func catalog(log *slog.Logger) []agent.Definition {
	defs, problems := agent.Catalog(builtins(), nil, nil)
	for _, p := range problems {
		log.Error("app: agent left out of the catalog", "agent", p.Agent, "problem", p.Problem.Code)
	}
	return defs
}
