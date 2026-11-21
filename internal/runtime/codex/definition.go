package codex

import (
	_ "embed"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/task"
)

// icon 是產品圖示，取自 ACP Registry 的 codex-acp 條目
// （來源與規則見 docs/architecture/agent.md「注册」）。
//
//go:embed icon.svg
var icon []byte

// Definition 是內建的 Codex Agent。其寬鬆模式透過
// app-server 的 thread 設定選擇，因此 Launch.Native 為空。
func Definition() agent.Definition {
	return agent.Definition{
		ID:       "codex",
		Name:     "Codex",
		Icon:     agent.IconURL(icon),
		Protocol: task.ProtocolCodex,
		Detect: agent.DetectSpec{
			Command: "codex",
			NPM:     []string{"@openai/codex"},
			Brew:    []string{"codex"},
		},
		Launch:  agent.LaunchSpec{Args: []string{"app-server"}},
		Version: &agent.VersionProbe{Args: []string{"--version"}, Pattern: `^codex-cli (\d+\.\d+\.\d+)`},
		// 已登入時以 0 結束。
		Auth: &agent.AuthProbe{Args: []string{"login", "status"}},
		Install: agent.InstallHint{
			DocsURL:  "https://github.com/openai/codex",
			Commands: map[string]string{"darwin": "brew install --cask codex", "linux": "npm install -g @openai/codex"},
		},
		Supersedes: []string{"codex-acp"},
		// 以 thread/resume 接回（CX02）。刪除原生會話 hub 還做不到，所以不宣告 DeleteNative（CX50）。
		Caps: task.Capabilities{Resume: true},
	}
}
