package pi

// usage.go：讀 pi 在本機的用量記錄（docs/architecture/runtime.md「本機用量記錄」，
// 已對照真機上的記錄核實）。

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// Usage 是讀 pi 本機用量記錄的 usage.Source。
func Usage() usage.Source { return source{} }

type source struct{}

func (source) Agent() task.AgentID          { return "pi" }
func (source) Unreported() []usage.Category { return nil }
func (source) Notes() []usage.Note          { return nil }

// Find 找 $PI_CODING_AGENT_DIR（預設 ~/.pi/agent）下 sessions/ 裡的 *.jsonl。
func (source) Find(env proc.Env) ([]string, []usage.File, error) {
	dir := usage.Root(env, "PI_CODING_AGENT_DIR", filepath.Join(".pi", "agent"))
	if dir == "" {
		return nil, nil, nil
	}
	root := filepath.Join(dir, "sessions")
	files, err := usage.Walk(root, func(path string) (usage.Format, bool) {
		return usage.Lines, strings.HasSuffix(path, ".jsonl")
	})
	return []string{root}, files, err
}

func (source) Parser(_ usage.File, state []byte) (usage.Parser, error) {
	p := &parser{}
	if len(state) > 0 {
		if err := json.Unmarshal(state, &p.s); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// state 是會話檔開頭那一行的會話 ID 與目錄。
type state struct {
	Session string `json:"session,omitzero"`
	Cwd     string `json:"cwd,omitzero"`
}

type parser struct{ s state }

func (p *parser) State() []byte {
	b, _ := json.Marshal(p.s)
	return b
}

type line struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Cwd       string    `json:"cwd"`
	Timestamp time.Time `json:"timestamp"`
	Message   *struct {
		Role      string `json:"role"`
		Model     string `json:"model"`
		Timestamp int64  `json:"timestamp"` // 毫秒
		Usage     *struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cacheRead"`
			CacheWrite int64 `json:"cacheWrite"`
			Total      int64 `json:"totalTokens"`
		} `json:"usage"`
	} `json:"message"`
}

func (p *parser) Line(b []byte) ([]usage.Record, []usage.Quota, bool) {
	if !usage.IsObject(b) {
		return nil, nil, true
	}
	header := bytes.Contains(b, []byte(`"session"`)) && p.s.Session == ""
	if !header && !bytes.Contains(b, []byte(`"usage"`)) {
		return nil, nil, false
	}
	var l line
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, nil, true
	}
	switch {
	case l.Type == "session":
		if p.s.Session == "" {
			p.s.Session, p.s.Cwd = l.ID, l.Cwd
		}
	case l.Type == "message" && l.Message != nil && l.Message.Role == "assistant" && l.Message.Usage != nil:
		u := l.Message.Usage
		t := usage.Tokens{Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output}
		if l.ID == "" || !t.Valid() || t.Total() != u.Total {
			return nil, nil, true
		}
		at := l.Timestamp
		if l.Message.Timestamp > 0 {
			at = time.UnixMilli(l.Message.Timestamp).UTC()
		}
		// 項目 ID 只在一個會話裡唯一；加上時間，分叉或複製出來的會話沿用同一項目時合成一條。
		key := l.ID + ":" + strconv.FormatInt(at.UnixMilli(), 10)
		return []usage.Record{{Key: key, At: at, Session: p.s.Session, Model: l.Message.Model,
			Dir: p.s.Cwd, Tokens: t}}, nil, false
	}
	return nil, nil, false
}
