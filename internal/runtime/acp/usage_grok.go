package acp

// usage_grok.go：讀 Grok 在本機的用量記錄（docs/architecture/runtime.md「本機用量
// 記錄」，已對照真機上的記錄核實）。

import (
	"bytes"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// GrokUsage 是讀 Grok 本機用量記錄的 usage.Source。
func GrokUsage() usage.Source { return grokSource{} }

type grokSource struct{}

func (grokSource) Agent() task.AgentID          { return grokID }
func (grokSource) Unreported() []usage.Category { return nil }
func (grokSource) Notes() []usage.Note          { return []usage.Note{usage.NoteTurnEnd} }

// Find 找 $GROK_HOME（預設 ~/.grok）下的 sessions/<URL 編碼的目錄>/<會話 ID>/updates.jsonl。
func (grokSource) Find(env proc.Env) ([]string, []usage.File, error) {
	home := usage.Root(env, "GROK_HOME", ".grok")
	if home == "" {
		return nil, nil, nil
	}
	root := filepath.Join(home, "sessions")
	files, err := usage.Walk(root, func(path string) (usage.Format, bool) {
		rel, err := filepath.Rel(root, path)
		return usage.Lines, err == nil && filepath.Base(path) == "updates.jsonl" &&
			strings.Count(filepath.ToSlash(rel), "/") == 2
	})
	return []string{root}, files, err
}

// Parser 的會話 ID 與目錄都從路徑來：目錄名是 URL 編碼的工作目錄。
func (grokSource) Parser(f usage.File, _ []byte) (usage.Parser, error) {
	session := filepath.Base(filepath.Dir(f.Path))
	dir, err := url.PathUnescape(filepath.Base(filepath.Dir(filepath.Dir(f.Path))))
	if err != nil {
		dir = ""
	}
	return grokParser{session: session, dir: dir}, nil
}

type grokParser struct{ session, dir string }

func (grokParser) State() []byte { return nil }

type grokCounts struct {
	Input      int64 `json:"inputTokens"`
	Output     int64 `json:"outputTokens"`
	Total      int64 `json:"totalTokens"`
	CacheRead  int64 `json:"cachedReadTokens"`
	CacheWrite int64 `json:"cacheCreationTokens"`
}

// tokens 換算四類：輸入含緩存（讀與寫），輸出含推理；總數必須等於輸入加輸出。
func (c grokCounts) tokens() (usage.Tokens, bool) {
	t := usage.Tokens{Input: c.Input - c.CacheRead - c.CacheWrite, CacheRead: c.CacheRead,
		CacheWrite: c.CacheWrite, Output: c.Output}
	return t, t.Valid() && c.Input+c.Output == c.Total
}

type grokLine struct {
	Timestamp int64 `json:"timestamp"` // 秒
	Params    struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind     string `json:"sessionUpdate"`
			PromptID string `json:"prompt_id"`
			Usage    *struct {
				grokCounts
				Models map[string]grokCounts `json:"modelUsage"`
			} `json:"usage"`
		} `json:"update"`
		Meta struct {
			At int64 `json:"agentTimestampMs"`
		} `json:"_meta"`
	} `json:"params"`
}

// Line 讀完成的一輪（turn_completed）：每個模型一條，Key 是會話、prompt 與模型。
func (p grokParser) Line(b []byte) ([]usage.Record, []usage.Quota, bool) {
	if !usage.IsObject(b) {
		return nil, nil, true
	}
	if !bytes.Contains(b, []byte(`"turn_completed"`)) {
		return nil, nil, false
	}
	var l grokLine
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, nil, true
	}
	up := l.Params.Update
	if up.Kind != "turn_completed" || up.Usage == nil {
		return nil, nil, false // 沒完成就被取消的一輪沒有用量
	}
	if up.PromptID == "" {
		return nil, nil, true
	}
	at := time.Unix(l.Timestamp, 0).UTC()
	if l.Params.Meta.At > 0 {
		at = time.UnixMilli(l.Params.Meta.At).UTC()
	}
	session := l.Params.SessionID
	if session == "" {
		session = p.session
	}
	models := up.Usage.Models
	if len(models) == 0 {
		models = map[string]grokCounts{"": up.Usage.grokCounts}
	}
	var recs []usage.Record
	for model, c := range models {
		t, ok := c.tokens()
		if !ok {
			return nil, nil, true
		}
		recs = append(recs, usage.Record{Key: session + ":" + up.PromptID + ":" + model, At: at,
			Session: session, Model: model, Dir: p.dir, Tokens: t})
	}
	return recs, nil, false
}
