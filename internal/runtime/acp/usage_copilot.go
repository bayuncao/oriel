package acp

// usage_copilot.go：讀 GitHub Copilot CLI 與 Cursor CLI 的本機用量記錄
// （docs/architecture/runtime.md「本機用量記錄」）。本機沒有 Copilot 的記錄，
// 格式取自 ccusage 的說明，待真機核實。

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// CopilotUsage 是讀 GitHub Copilot CLI 本機用量記錄的 usage.Source。
func CopilotUsage() usage.Source { return copilotSource{} }

type copilotSource struct{}

func (copilotSource) Agent() task.AgentID          { return "copilot" }
func (copilotSource) Unreported() []usage.Category { return nil }

// Notes：會話結束時才寫下用量；被強制結束的進程，那段用量沒有記錄。
func (copilotSource) Notes() []usage.Note { return []usage.Note{usage.NoteSessionEnd} }

// Find 找 $COPILOT_HOME（預設 ~/.copilot）下的 session-state/<會話 ID>/events.jsonl。
func (copilotSource) Find(env proc.Env) ([]string, []usage.File, error) {
	home := usage.Root(env, "COPILOT_HOME", ".copilot")
	if home == "" {
		return nil, nil, nil
	}
	root := filepath.Join(home, "session-state")
	files, err := usage.Walk(root, func(path string) (usage.Format, bool) {
		return usage.Lines, filepath.Base(path) == "events.jsonl" && filepath.Dir(filepath.Dir(path)) == root
	})
	return []string{root}, files, err
}

func (copilotSource) Parser(f usage.File, state []byte) (usage.Parser, error) {
	p := &copilotParser{session: filepath.Base(filepath.Dir(f.Path))}
	if len(state) > 0 {
		if err := json.Unmarshal(state, &p.s); err != nil {
			return nil, err
		}
	}
	if p.s.Last == nil {
		p.s.Last = map[string]copilotCounts{}
	}
	return p, nil
}

// copilotState 是讀到第幾次 shutdown，與上一次每個模型的累計數。
type copilotState struct {
	Shutdowns int                      `json:"shutdowns"`
	Last      map[string]copilotCounts `json:"last"`
}

type copilotParser struct {
	session string
	s       copilotState
}

func (p *copilotParser) State() []byte {
	b, _ := json.Marshal(p.s)
	return b
}

type copilotCounts struct {
	Input      int64 `json:"inputTokens"`
	Output     int64 `json:"outputTokens"`
	CacheRead  int64 `json:"cacheReadTokens"`
	CacheWrite int64 `json:"cacheWriteTokens"`
}

type copilotLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Data      struct {
		Models map[string]struct {
			Usage *copilotCounts `json:"usage"`
		} `json:"modelMetrics"`
	} `json:"data"`
}

// Line 讀 session.shutdown：每個模型的累計數減去同一會話上一次 shutdown 的；比上
// 一次少時（舊版本每次恢復都從零記）按原值算。輸入含緩存（讀與寫）。
func (p *copilotParser) Line(b []byte) ([]usage.Record, []usage.Quota, bool) {
	if !usage.IsObject(b) {
		return nil, nil, true
	}
	if !bytes.Contains(b, []byte(`"session.shutdown"`)) {
		return nil, nil, false
	}
	var l copilotLine
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, nil, true
	}
	if l.Type != "session.shutdown" {
		return nil, nil, false
	}
	p.s.Shutdowns++
	models := make([]string, 0, len(l.Data.Models))
	for m := range l.Data.Models {
		models = append(models, m)
	}
	slices.Sort(models)
	var recs []usage.Record
	bad := false
	for _, model := range models {
		cur := l.Data.Models[model].Usage
		if cur == nil {
			continue
		}
		used := *cur
		if prev, ok := p.s.Last[model]; ok {
			d := copilotCounts{cur.Input - prev.Input, cur.Output - prev.Output,
				cur.CacheRead - prev.CacheRead, cur.CacheWrite - prev.CacheWrite}
			if d.Input >= 0 && d.Output >= 0 && d.CacheRead >= 0 && d.CacheWrite >= 0 {
				used = d
			}
		}
		p.s.Last[model] = *cur
		t := usage.Tokens{Input: used.Input - used.CacheRead - used.CacheWrite, CacheRead: used.CacheRead,
			CacheWrite: used.CacheWrite, Output: used.Output}
		if !t.Valid() {
			bad = true
			continue
		}
		key := p.session + ":" + model + ":" + strconv.Itoa(p.s.Shutdowns)
		recs = append(recs, usage.Record{Key: key, At: l.Timestamp, Session: p.session, Model: model, Tokens: t})
	}
	return recs, nil, bad
}

// CursorUsage 是 Cursor CLI 的 usage.Source：它不在本機記 token，只在記錄來源裡
// 說明這一點，不讀任何檔案。要它的用量只能用登入 cookie 呼叫 Cursor 的網頁介面，
// 違反“不碰各 agent 的登入憑據”（D88）。
func CursorUsage() usage.Source { return cursorSource{} }

type cursorSource struct{}

func (cursorSource) Agent() task.AgentID          { return "cursor" }
func (cursorSource) Unreported() []usage.Category { return nil }
func (cursorSource) Notes() []usage.Note {
	return []usage.Note{usage.NoteNotRecorded, usage.NoteReportedOnly}
}
func (cursorSource) Find(proc.Env) ([]string, []usage.File, error) {
	return nil, nil, nil
}
func (cursorSource) Parser(usage.File, []byte) (usage.Parser, error) {
	return nil, errors.New("acp: cursor keeps no usage records")
}
