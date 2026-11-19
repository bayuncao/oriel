package acp

// usage_gemini.go：讀 Gemini CLI 與 Qwen Code 在本機的用量記錄（docs/architecture/
// runtime.md「本機用量記錄」）。本機沒有這兩種記錄，格式取自 ccusage 的說明，
// 待真機核實。

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

// GeminiUsage 是讀 Gemini CLI 本機用量記錄的 usage.Source。
func GeminiUsage() usage.Source { return geminiSource{} }

type geminiSource struct{}

func (geminiSource) Agent() task.AgentID { return "gemini" }

// Unreported：Gemini 只報緩存讀取（cached），沒有緩存寫入。
func (geminiSource) Unreported() []usage.Category { return []usage.Category{usage.CacheWrite} }
func (geminiSource) Notes() []usage.Note          { return nil }

// Find 找 $GEMINI_CLI_HOME（預設家目錄）下 .gemini/tmp/*/chats/ 裡的會話：
// *.json 是整份改寫的會話檔，*.jsonl 只追加。
func (geminiSource) Find(env proc.Env) ([]string, []usage.File, error) {
	home := usage.Root(env, "GEMINI_CLI_HOME", "")
	if home == "" {
		return nil, nil, nil
	}
	root := filepath.Join(home, ".gemini", "tmp")
	files, err := usage.Walk(root, func(path string) (usage.Format, bool) {
		if filepath.Base(filepath.Dir(path)) != "chats" {
			return "", false
		}
		switch filepath.Ext(path) {
		case ".json":
			return usage.Whole, true
		case ".jsonl":
			return usage.Lines, true
		}
		return "", false
	})
	return []string{root}, files, err
}

func (geminiSource) Parser(f usage.File, state []byte) (usage.Parser, error) {
	p := &geminiParser{whole: f.Format == usage.Whole}
	if len(state) > 0 {
		if err := json.Unmarshal(state, &p.session); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// geminiParser 的狀態是會話 ID（JSONL 的開頭一行）。
type geminiParser struct {
	whole   bool
	session string
}

func (p *geminiParser) State() []byte {
	b, _ := json.Marshal(p.session)
	return b
}

type geminiMessage struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Model     string    `json:"model"`
	Tokens    *struct {
		Input    int64 `json:"input"`
		Output   int64 `json:"output"`
		Cached   int64 `json:"cached"`
		Thoughts int64 `json:"thoughts"`
		Tool     int64 `json:"tool"`
		Total    int64 `json:"total"`
	} `json:"tokens"`
}

// record 換算四類：輸入含 cached，tool 是另外的輸入，thoughts 是另外的輸出；
// 四類之和必須等於 total。
func (m geminiMessage) record(session string) (usage.Record, bool, bool) {
	if m.Tokens == nil {
		return usage.Record{}, false, false
	}
	t := m.Tokens
	tokens := usage.Tokens{Input: t.Input - t.Cached + t.Tool, CacheRead: t.Cached, Output: t.Output + t.Thoughts}
	if m.ID == "" || !tokens.Valid() || (t.Total != 0 && tokens.Total() != t.Total) {
		return usage.Record{}, false, true
	}
	return usage.Record{Key: session + ":" + m.ID, At: m.Timestamp, Session: session, Model: m.Model, Tokens: tokens}, true, false
}

func (p *geminiParser) Line(b []byte) ([]usage.Record, []usage.Quota, bool) {
	if !usage.IsObject(b) {
		return nil, nil, true
	}
	if p.whole {
		var f struct {
			SessionID string          `json:"sessionId"`
			Messages  []geminiMessage `json:"messages"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, nil, true
		}
		var recs []usage.Record
		bad := false
		for _, m := range f.Messages {
			r, ok, isBad := m.record(f.SessionID)
			bad = bad || isBad
			if ok {
				recs = append(recs, r)
			}
		}
		return recs, nil, bad
	}
	if !bytes.Contains(b, []byte(`"tokens"`)) && !(p.session == "" && bytes.Contains(b, []byte(`"sessionId"`))) {
		return nil, nil, false
	}
	var m geminiMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, nil, true
	}
	if p.session == "" {
		p.session = m.SessionID
	}
	r, ok, bad := m.record(p.session)
	if !ok {
		return nil, nil, bad
	}
	return []usage.Record{r}, nil, false
}

// QwenUsage 是讀 Qwen Code 本機用量記錄的 usage.Source。
func QwenUsage() usage.Source { return qwenSource{} }

type qwenSource struct{}

func (qwenSource) Agent() task.AgentID { return "qwen" }

// Unreported：Qwen 的記錄沒有緩存寫入。
func (qwenSource) Unreported() []usage.Category { return []usage.Category{usage.CacheWrite} }
func (qwenSource) Notes() []usage.Note          { return nil }

// Find 找 ~/.qwen/projects/*/chats/*.jsonl，舊版在 ~/.qwen/tmp/*/chats/。
func (qwenSource) Find(env proc.Env) ([]string, []usage.File, error) {
	home := usage.Root(env, "", ".qwen")
	if home == "" {
		return nil, nil, nil
	}
	roots := []string{filepath.Join(home, "projects"), filepath.Join(home, "tmp")}
	var files []usage.File
	for _, root := range roots {
		found, err := usage.Walk(root, func(path string) (usage.Format, bool) {
			return usage.Lines, strings.HasSuffix(path, ".jsonl") && filepath.Base(filepath.Dir(path)) == "chats"
		})
		files = append(files, found...)
		if err != nil {
			return roots, files, err
		}
	}
	return roots, files, nil
}

func (qwenSource) Parser(usage.File, []byte) (usage.Parser, error) { return qwenParser{}, nil }

type qwenParser struct{}

func (qwenParser) State() []byte { return nil }

type qwenLine struct {
	UUID      string    `json:"uuid"`
	SessionID string    `json:"sessionId"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Cwd       string    `json:"cwd"`
	Model     string    `json:"model"`
	Usage     *struct {
		Prompt     int64 `json:"promptTokenCount"`
		Candidates int64 `json:"candidatesTokenCount"`
		Thoughts   int64 `json:"thoughtsTokenCount"`
		Cached     int64 `json:"cachedContentTokenCount"`
		Tool       int64 `json:"toolUsePromptTokenCount"`
		Total      int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// Line 讀帶 usageMetadata 的 assistant 行：prompt 含 cached，thoughts 是另外的輸出。
func (qwenParser) Line(b []byte) ([]usage.Record, []usage.Quota, bool) {
	if !usage.IsObject(b) {
		return nil, nil, true
	}
	if !bytes.Contains(b, []byte(`"usageMetadata"`)) {
		return nil, nil, false
	}
	var l qwenLine
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, nil, true
	}
	if l.Type != "assistant" || l.Usage == nil {
		return nil, nil, false
	}
	u := l.Usage
	t := usage.Tokens{Input: u.Prompt - u.Cached + u.Tool, CacheRead: u.Cached, Output: u.Candidates + u.Thoughts}
	if l.UUID == "" || !t.Valid() || (u.Total != 0 && t.Total() != u.Total) {
		return nil, nil, true
	}
	return []usage.Record{{Key: l.UUID, At: l.Timestamp, Session: l.SessionID, Model: l.Model,
		Dir: l.Cwd, Tokens: t}}, nil, false
}
