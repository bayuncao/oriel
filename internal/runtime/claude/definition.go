package claude

import (
	_ "embed"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/task"
)

// icon 是產品圖示，取自 ACP Registry 的 claude-acp 條目
// （來源與規則見 docs/architecture/agent.md「注册」）。
//
//go:embed icon.svg
var icon []byte

// Definition 是內建的 Claude Code Agent。兩種啟動都明確指定
// --permission-mode：沒指定時起始模式取決於設定與版本（有的版本
// 以 auto 啟動），工具呼叫會在詢問 OrielHub 之前就被放行。見
// docs/research/claude-control-protocol.md。
func Definition() agent.Definition {
	// 字面值的容量等於長度，兩次 append 各自配置新陣列。--include-partial-messages 讓文字邊寫邊送（CL13）；
	// 會話 ID（--session-id / --resume）由轉接器在啟動時加上。
	base := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--permission-prompt-tool", "stdio"}
	return agent.Definition{
		ID:       "claude",
		Name:     "Claude Code",
		Icon:     agent.IconURL(icon),
		Protocol: task.ProtocolClaude,
		Detect: agent.DetectSpec{
			Command: "claude",
			Dirs:    []string{"~/.claude/local"}, // 舊的單使用者安裝位置
			NPM:     []string{"@anthropic-ai/claude-code"},
			Brew:    []string{"claude-code"},
		},
		Launch: agent.LaunchSpec{
			Args:   append(base, "--permission-mode", "default"),
			Native: append(base, "--permission-mode", "bypassPermissions"),
		},
		Version: &agent.VersionProbe{Args: []string{"--version"}, Pattern: `^(\d+\.\d+\.\d+) \(Claude Code\)`},
		// 登出時印出 JSON 並以 1 結束。
		Auth: &agent.AuthProbe{Args: []string{"auth", "status"}, AuthenticatedRE: `"loggedIn":\s*true`},
		Install: agent.InstallHint{
			DocsURL: "https://claude.com/product/claude-code",
			// npm 安裝已被官方棄用；原生安裝程式放在 ~/.local/bin。
			Commands: map[string]string{"darwin": "brew install --cask claude-code", "linux": "curl -fsSL https://claude.ai/install.sh | bash"},
		},
		Supersedes: []string{"claude-acp"},
		// 以 --resume 接回同一個會話 ID（CL02）。刪除原生會話檔 hub 還做不到，所以不宣告 DeleteNative。
		Caps: task.Capabilities{Resume: true},
	}
}
