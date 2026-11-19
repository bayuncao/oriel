package acp

import (
	_ "embed"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/task"
)

// copilotIcon 是產品圖示，取自 ACP Registry 的 github-copilot-cli 條目
// （來源與規則見 docs/architecture/agent.md「注册」）。
//
//go:embed icons/copilot.svg
var copilotIcon []byte

// Copilot 是內建的 GitHub Copilot CLI Agent，以 `copilot --acp`
// 使用 ACP。Homebrew cask 裝的是原生程式，靠版本輸出確認身分
// （AWS 的同名 copilot 印出的格式不同）。登入只有互動式的
// `copilot login`，因此登入狀態為未知。
func Copilot() agent.Definition {
	return agent.Definition{
		ID:       "copilot",
		Name:     "GitHub Copilot CLI",
		Icon:     agent.IconURL(copilotIcon),
		Protocol: task.ProtocolACP,
		Detect: agent.DetectSpec{
			Command: "copilot",
			NPM:     []string{"@github/copilot"},
			Brew:    []string{"copilot-cli"},
		},
		Launch: agent.LaunchSpec{
			Args:   []string{"--acp"},
			Native: []string{"--acp", "--allow-all"},
		},
		Version: &agent.VersionProbe{Args: []string{"--version"}, Pattern: `^GitHub Copilot CLI (\d+\.\d+\.\d+)`},
		Install: agent.InstallHint{
			DocsURL:  "https://github.com/github/copilot-cli",
			Commands: map[string]string{"darwin": "brew install --cask copilot-cli", "linux": "npm install -g @github/copilot"},
		},
		Supersedes: []string{"github-copilot-cli"},
	}
}
