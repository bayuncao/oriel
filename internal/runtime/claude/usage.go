package claude

// usage.go：讀 Claude Code 在本機的用量記錄（docs/architecture/runtime.md
// 「本機用量記錄」，已對照真機上的記錄核實）。

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// claudeAgent 是 Claude Code 的 AgentID：本機記錄與協議報來的額度都記在它名下。
const claudeAgent task.AgentID = "claude"

// Usage 是讀 Claude Code 本機用量記錄的 usage.Source。
func Usage() usage.Source { return source{} }

type source struct{}

func (source) Agent() task.AgentID                             { return claudeAgent }
func (source) Unreported() []usage.Category                    { return nil }
func (source) Notes() []usage.Note                             { return nil }
func (source) Parser(usage.File, []byte) (usage.Parser, error) { return parser{}, nil }

// Find 找 $CLAUDE_CONFIG_DIR（沒設時是 ~/.claude 與 $XDG_CONFIG_HOME/claude）
// 下 projects 裡的 *.jsonl；子 agent 的記錄在 <會話>/subagents/ 裡，一併找。
func (source) Find(env proc.Env) ([]string, []usage.File, error) {
	var roots []string
	if dir := env.Get("CLAUDE_CONFIG_DIR"); dir != "" {
		roots = []string{filepath.Join(dir, "projects")}
	} else if home := env.Home(); home != "" {
		config := env.Get("XDG_CONFIG_HOME")
		if config == "" {
			config = filepath.Join(home, ".config")
		}
		roots = []string{filepath.Join(home, ".claude", "projects"), filepath.Join(config, "claude", "projects")}
	}
	var files []usage.File
	for _, root := range roots {
		found, err := usage.Walk(root, func(path string) (usage.Format, bool) {
			return usage.Lines, strings.HasSuffix(path, ".jsonl")
		})
		files = append(files, found...)
		if err != nil {
			return roots, files, err
		}
	}
	return roots, files, nil
}

// line 是記錄裡帶用量的那種行（type 為 assistant）。
type line struct {
	Type      string    `json:"type"`
	SessionID string    `json:"sessionId"`
	Cwd       string    `json:"cwd"`
	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"requestId"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			counts
			// Iterations 是一次回應裡的各次迭代；有的行頂層是 0 而數字只在這裡。
			Iterations []counts `json:"iterations"`
		} `json:"usage"`
	} `json:"message"`
}

type counts struct {
	Input      int64 `json:"input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	Output     int64 `json:"output_tokens"`
}

func (c counts) tokens() usage.Tokens {
	return usage.Tokens{Input: c.Input, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite, Output: c.Output}
}

// parser 不需要狀態：每行都帶著會話、目錄與模型。
type parser struct{}

func (parser) State() []byte { return nil }

// Line 讀一次 API 回應的用量。一次回應會寫好幾行，Key 相同（message.id 加
// requestId），合併時每類取較大值；會話分叉時歷史被複製進新檔案，Key 也相同。
func (parser) Line(b []byte) ([]usage.Record, []usage.Quota, bool) {
	if !usage.IsObject(b) {
		return nil, nil, true
	}
	if !bytes.Contains(b, []byte(`"usage"`)) || !bytes.Contains(b, []byte(`"assistant"`)) {
		return nil, nil, false
	}
	var l line
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, nil, true
	}
	if l.Type != "assistant" || l.Message == nil || l.Message.Usage == nil {
		return nil, nil, false
	}
	if l.Message.ID == "" {
		return nil, nil, true
	}
	u := l.Message.Usage
	tokens := u.tokens()
	var sum usage.Tokens
	for _, it := range u.Iterations {
		sum = sum.Add(it.tokens())
	}
	tokens = tokens.Max(sum)
	if !tokens.Valid() {
		return nil, nil, true
	}
	key := l.Message.ID
	if l.RequestID != "" {
		key += ":" + l.RequestID
	}
	return []usage.Record{{Key: key, At: l.Timestamp, Session: l.SessionID, Model: l.Message.Model,
		Dir: l.Cwd, Tokens: tokens}}, nil, false
}
