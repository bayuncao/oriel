package acp

import (
	_ "embed"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/task"
)

// geminiIcon 是產品圖示，取自 ACP Registry 的 gemini 條目
// （來源與規則見 docs/architecture/agent.md「注册」）。
//
//go:embed icons/gemini.svg
var geminiIcon []byte

// Gemini 是內建的 Gemini CLI Agent，以 `gemini --acp` 使用 ACP。
// npm 與 Homebrew 的安裝都從 package.json 讀版本；`gemini
// --version` 印出裸版本號。Gemini 沒有登入狀態命令。
func Gemini() agent.Definition {
	return agent.Definition{
		ID:       "gemini",
		Name:     "Gemini CLI",
		Icon:     agent.IconURL(geminiIcon),
		Protocol: task.ProtocolACP,
		Detect: agent.DetectSpec{
			Command: "gemini",
			NPM:     []string{"@google/gemini-cli"},
			Brew:    []string{"gemini-cli"},
		},
		Launch: agent.LaunchSpec{
			Args:   []string{"--acp"},
			Native: []string{"--acp", "--yolo"},
		},
		Version: &agent.VersionProbe{Args: []string{"--version"}, Pattern: agent.BareVersion},
		Install: agent.InstallHint{
			DocsURL:  "https://github.com/google-gemini/gemini-cli",
			Commands: map[string]string{"darwin": "brew install gemini-cli", "linux": "npm install -g @google/gemini-cli"},
		},
		Supersedes: []string{"gemini"},
	}
}
