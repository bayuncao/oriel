package acp

import (
	_ "embed"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/task"
)

// grokID 是內建 Grok 的 Agent ID。
const grokID = "grok"

// grokAsk 是建立與接回會話時帶的 _meta：使用者設定裡的
// permission_mode = "always-approve" 會讓 Grok 不經 OrielHub 就執行工具，
// yoloMode 為 false 讓這個會話照常詢問（D101）。接回時也要帶，否則恢復成
// 設定裡的模式。2026-11-05 在 1.0.70 上核實；GROK_CONFIG 蓋不掉這個設定。
func grokAsk() map[string]any { return map[string]any{"yoloMode": false} }

// grokIcon 是產品圖示，取自 ACP Registry 的 grok-build 條目
// （來源與規則見 docs/architecture/agent.md「注册」）。
//
//go:embed icons/grok.svg
var grokIcon []byte

// Grok 是內建的 Grok Build Agent，使用 ACP。Homebrew 的 grok
// formula 是個無關的 regex 工具，因此 PATH 上的 grok 必須來自
// grok-build cask。Grok 沒有登入狀態命令。
func Grok() agent.Definition {
	return agent.Definition{
		ID:       grokID,
		Name:     "Grok",
		Icon:     agent.IconURL(grokIcon),
		Protocol: task.ProtocolACP,
		Detect: agent.DetectSpec{
			Command:    "grok",
			Dirs:       []string{"$GROK_BIN_DIR", "~/.grok/bin"},
			Brew:       []string{"grok-build"},
			NameShared: true,
		},
		Launch: agent.LaunchSpec{
			Args:   []string{"agent", "stdio"},
			Native: []string{"agent", "--always-approve", "stdio"},
		},
		Version: &agent.VersionProbe{Args: []string{"--version"}, Pattern: `^grok (\d+\.\d+\.\d+) \([0-9a-f]+\)`},
		Install: agent.InstallHint{
			DocsURL:  "https://x.ai/build",
			Commands: map[string]string{"darwin": "brew install --cask grok-build"},
		},
		Supersedes: []string{"grok-build"},
	}
}
