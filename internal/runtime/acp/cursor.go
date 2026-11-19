package acp

import (
	_ "embed"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/task"
)

// cursorIcon 是產品圖示，取自 ACP Registry 的 cursor 條目
// （來源與規則見 docs/architecture/agent.md「注册」）。
//
//go:embed icons/cursor.svg
var cursorIcon []byte

// Cursor 是內建的 Cursor CLI Agent，以 `cursor-agent acp` 使用
// ACP。官方安裝腳本在 ~/.local/bin 放符號連結；版本是日期加
// commit（"2026.10.19-6bbf2c4"）。`cursor-agent status` 一律以 0
// 結束，已登入時印出 "Logged in as"。
func Cursor() agent.Definition {
	return agent.Definition{
		ID:       "cursor",
		Name:     "Cursor CLI",
		Icon:     agent.IconURL(cursorIcon),
		Protocol: task.ProtocolACP,
		Detect: agent.DetectSpec{
			Command: "cursor-agent",
			Brew:    []string{"cursor-cli"},
		},
		Launch: agent.LaunchSpec{
			Args:   []string{"acp"},
			Native: []string{"--force", "acp"},
		},
		Version: &agent.VersionProbe{Args: []string{"--version"}, Pattern: `^(\d{4}\.\d{1,2}\.\d{1,2})-[0-9a-f]+(?:\n|$)`},
		Auth:    &agent.AuthProbe{Args: []string{"status"}, AuthenticatedRE: `Logged in as`},
		Install: agent.InstallHint{
			DocsURL:  "https://cursor.com/cli",
			Commands: map[string]string{"darwin": "curl https://cursor.com/install -fsS | bash", "linux": "curl https://cursor.com/install -fsS | bash"},
		},
		Supersedes: []string{"cursor"},
	}
}
