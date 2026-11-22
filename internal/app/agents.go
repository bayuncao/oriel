package app

import (
	"log/slog"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/runtime/acp"
	"github.com/bayuncao/oriel/internal/runtime/claude"
	"github.com/bayuncao/oriel/internal/runtime/codex"
	"github.com/bayuncao/oriel/internal/runtime/pi"
	"github.com/bayuncao/oriel/internal/store"
	"github.com/bayuncao/oriel/internal/task"
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

// newRuntimes 是 hub 的各協議轉接器；ACP 的另由 launcher 用來探測能力（D98），所以由呼叫者傳入。
// quota 收下協議報來的額度快照（usage.Tracker.Report，D87）。
func newRuntimes(runner *proc.Runner, version string, log *slog.Logger, acpRuntime *acp.Runtime, quota func(usage.Quota)) map[task.Protocol]hub.Runtime {
	return map[task.Protocol]hub.Runtime{
		task.ProtocolACP:    acpRuntime,
		task.ProtocolClaude: claude.New(claude.Options{Runner: runner, Logger: log, Quota: quota}),
		task.ProtocolCodex:  codex.New(codex.Options{Runner: runner, Version: version, Logger: log}),
		task.ProtocolPi:     pi.New(pi.Options{Runner: runner, Logger: log}),
	}
}

// catalog 是經 agent.Catalog 合併的目錄（標上 Source 與 Launchable：內建、且 runtimes 有它協議的轉接器，
// m3.md CX53）；M1 只有 builtin。每次呼叫都是新的一份，discovery 與 Agents 服務不共用切片。
func catalog(log *slog.Logger, runtimes map[task.Protocol]hub.Runtime) []agent.Definition {
	defs, problems := agent.Catalog(builtins(), nil, nil)
	for _, p := range problems {
		log.Error("app: agent left out of the catalog", "agent", p.Agent, "problem", p.Problem.Code)
	}
	for i, d := range defs {
		// 建 Task 的解析只認內建的 Definition（launcher.go）。
		defs[i].Launchable = d.Source == agent.SourceBuiltin && runtimes[d.Protocol] != nil
	}
	return defs
}
