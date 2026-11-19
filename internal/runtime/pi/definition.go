package pi

import (
	_ "embed"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/task"
)

// icon 是產品圖示，取自 ACP Registry 的 pi-acp 條目
// （來源與規則見 docs/architecture/agent.md「注册」）。
//
//go:embed icon.svg
var icon []byte

// Definition 是內建的 pi Agent。pi 以 npm 套件發行（也在
// Homebrew formula 內），因此其版本通常來自
// package.json；`pi --version` 印出裸版本號。`pi auth check`
// 需要 provider，因此登入狀態為未知。
func Definition() agent.Definition {
	return agent.Definition{
		ID:       "pi",
		Name:     "pi",
		Icon:     agent.IconURL(icon),
		Protocol: task.ProtocolPi,
		Detect: agent.DetectSpec{
			Command: "pi",
			NPM:     []string{"@earendil-works/pi-coding-agent", "@mariozechner/pi-coding-agent"},
			Brew:    []string{"pi-coding-agent"},
		},
		Launch:  agent.LaunchSpec{Args: []string{"--mode", "rpc"}},
		Version: &agent.VersionProbe{Args: []string{"--version"}, Pattern: agent.BareVersion},
		Install: agent.InstallHint{
			DocsURL:  "https://pi.dev/",
			Commands: map[string]string{"darwin": "brew install pi-coding-agent", "linux": "npm install -g @earendil-works/pi-coding-agent"},
		},
		Supersedes: []string{"pi-acp"},
	}
}
