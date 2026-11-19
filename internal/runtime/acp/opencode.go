package acp

import (
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
)

// openCodeID 是內建 OpenCode 的 Agent ID。
const openCodeID = "opencode"

// openCodeConfig 是 OpenCode 優先級最高的使用者設定（高於專案裡的 opencode.json）。
const openCodeConfig = "OPENCODE_CONFIG_CONTENT"

// openCodeAsk 讓 OpenCode 的編輯、shell 與網頁請求每次都詢問（D101）：
// 把 permission 合併進 OPENCODE_CONFIG_CONTENT，原有的其他設定不變。
func openCodeAsk(env proc.Env) (proc.Env, error) {
	cfg := map[string]any{}
	if v, ok := env.Lookup(openCodeConfig); ok && strings.TrimSpace(v) != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			return nil, fail(CodeOpenCodeConfig, err, "detail", err.Error())
		}
		if m == nil {
			return nil, fail(CodeOpenCodeConfig, nil, "detail", "null")
		}
		cfg = m
	}
	perm, _ := cfg["permission"].(map[string]any)
	if perm == nil {
		perm = map[string]any{}
	}
	for _, k := range []string{"edit", "bash", "webfetch"} {
		perm[k] = "ask"
	}
	cfg["permission"] = perm
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fail(CodeOpenCodeConfig, err, "detail", err.Error())
	}
	return env.With(openCodeConfig + "=" + string(b)), nil
}

// opencodeIcon 是產品圖示，取自 ACP Registry 的 opencode 條目
// （來源與規則見 docs/architecture/agent.md「注册」）。
//
//go:embed icons/opencode.svg
var opencodeIcon []byte

// OpenCode 是內建的 OpenCode Agent，以 `opencode acp` 使用 ACP。
// 官方安裝腳本放在 ~/.opencode/bin；`opencode --version` 印出
// 裸版本號，舊的同名 Go 版 opencode 印出的格式不同。`acp`
// 沒有寬鬆模式旗標；登入分屬各 provider，因此登入狀態為未知。
func OpenCode() agent.Definition {
	return agent.Definition{
		ID:       openCodeID,
		Name:     "OpenCode",
		Icon:     agent.IconURL(opencodeIcon),
		Protocol: task.ProtocolACP,
		Detect: agent.DetectSpec{
			Command: "opencode",
			Dirs:    []string{"~/.opencode/bin"},
			NPM:     []string{"opencode-ai"},
			Brew:    []string{"opencode"},
		},
		Launch:  agent.LaunchSpec{Args: []string{"acp"}},
		Version: &agent.VersionProbe{Args: []string{"--version"}, Pattern: agent.BareVersion},
		Install: agent.InstallHint{
			DocsURL:  "https://opencode.ai",
			Commands: map[string]string{"darwin": "brew install opencode", "linux": "npm install -g opencode-ai"},
		},
		Supersedes: []string{"opencode", "opencode-v2"},
	}
}
