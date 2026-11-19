package acp

import (
	_ "embed"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/task"
)

// qwenIcon 是產品圖示，取自 ACP Registry 的 qwen-code 條目
// （來源與規則見 docs/architecture/agent.md「注册」）。
//
//go:embed icons/qwen.svg
var qwenIcon []byte

// Qwen 是內建的 Qwen Code Agent，以 `qwen --acp` 使用 ACP。
// `qwen --version` 印出裸版本號；Qwen 沒有登入狀態命令。
func Qwen() agent.Definition {
	return agent.Definition{
		ID:       "qwen",
		Name:     "Qwen Code",
		Icon:     agent.IconURL(qwenIcon),
		Protocol: task.ProtocolACP,
		Detect: agent.DetectSpec{
			Command: "qwen",
			NPM:     []string{"@qwen-code/qwen-code"},
			Brew:    []string{"qwen-code"},
		},
		Launch: agent.LaunchSpec{
			Args:   []string{"--acp"},
			Native: []string{"--acp", "--yolo"},
		},
		Version: &agent.VersionProbe{Args: []string{"--version"}, Pattern: agent.BareVersion},
		Install: agent.InstallHint{
			DocsURL:  "https://github.com/QwenLM/qwen-code",
			Commands: map[string]string{"darwin": "brew install qwen-code", "linux": "npm install -g @qwen-code/qwen-code"},
		},
		Supersedes: []string{"qwen-code"},
	}
}
