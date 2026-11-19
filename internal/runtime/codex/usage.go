package codex

// usage.go：讀 Codex 在本機的用量記錄與額度快照（docs/architecture/runtime.md
// 「本機用量記錄」，已對照真機上的 4,000 多個 rollout 核實）。

import (
	"bytes"
	"cmp"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// Usage 是讀 Codex 本機用量記錄的 usage.Source。
func Usage() usage.Source { return source{} }

type source struct{}

func (source) Agent() task.AgentID          { return "codex" }
func (source) Unreported() []usage.Category { return nil }
func (source) Notes() []usage.Note          { return nil }

// Find 找 $CODEX_HOME（預設 ~/.codex）下 sessions/ 與 archived_sessions/ 裡的
// rollout。壓縮的 .jsonl.zst 照讀（D91）；同一目錄裡有未壓縮的那份時只讀它，
// 與 Codex 自己相同。
func (source) Find(env proc.Env) ([]string, []usage.File, error) {
	home := usage.Root(env, "CODEX_HOME", ".codex")
	if home == "" {
		return nil, nil, nil
	}
	roots := []string{filepath.Join(home, "sessions"), filepath.Join(home, "archived_sessions")}
	var files []usage.File
	for _, root := range roots {
		found, err := usage.Walk(root, func(path string) (usage.Format, bool) {
			name := filepath.Base(path)
			if !strings.HasPrefix(name, "rollout-") {
				return "", false
			}
			switch {
			case strings.HasSuffix(name, ".jsonl"):
				return usage.Lines, true
			case strings.HasSuffix(name, ".jsonl.zst"):
				return usage.Compressed, true
			}
			return "", false
		})
		plain := map[string]bool{}
		for _, f := range found {
			if f.Format == usage.Lines {
				plain[f.Path] = true
			}
		}
		for _, f := range found {
			if f.Format == usage.Compressed && plain[strings.TrimSuffix(f.Path, ".zst")] {
				continue
			}
			files = append(files, f)
		}
		if err != nil {
			return roots, files, err
		}
	}
	return roots, files, nil
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

// state 是 rollout 的解析狀態：線程、目錄、模型等中繼資料與上一個累計數，不含對話內容。
type state struct {
	Thread string `json:"thread,omitzero"`
	Parent string `json:"parent,omitzero"`
	Cwd    string `json:"cwd,omitzero"`
	Model  string `json:"model,omitzero"`
	// Start 是子線程自己的第一行（subagent_history_start_ordinal）；之前的是重放的父線程歷史。
	Start int64 `json:"start,omitzero"`
	// Records 表示已見過 token_usage_record：從它起只算它，不再算 token_count
	// （記錄寫在它的 token_count 之前，所以邊界上不會重複）。
	Records bool `json:"records,omitzero"`
	// Total 是上一個 token_count 的線程累計數。
	Total *counts `json:"total,omitzero"`
}

type parser struct{ s state }

func (p *parser) State() []byte {
	b, _ := json.Marshal(p.s)
	return b
}

type counts struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
	Total      int64 `json:"total_tokens"`
}

func (c counts) minus(o counts) counts {
	return counts{c.Input - o.Input, c.Cached - o.Cached, c.CacheWrite - o.CacheWrite,
		c.Output - o.Output, c.Reasoning - o.Reasoning, c.Total - o.Total}
}

// tokens 換算四類：輸入含緩存（讀與寫），輸出含推理；總數必須等於輸入加輸出。
func (c counts) tokens() (usage.Tokens, bool) {
	t := usage.Tokens{Input: c.Input - c.Cached - c.CacheWrite, CacheRead: c.Cached,
		CacheWrite: c.CacheWrite, Output: c.Output}
	return t, t.Valid() && c.Input+c.Output == c.Total
}

type line struct {
	Timestamp time.Time       `json:"timestamp"`
	Ordinal   *int64          `json:"ordinal"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type meta struct {
	ID     string `json:"id"`
	Parent string `json:"parent_thread_id"`
	Cwd    string `json:"cwd"`
	Start  *int64 `json:"subagent_history_start_ordinal"`
}

type turnContext struct {
	Cwd   string `json:"cwd"`
	Model string `json:"model"`
}

type record struct {
	Thread   string  `json:"thread_id"`
	Response string  `json:"response_id"`
	Usage    *counts `json:"usage"`
}

type tokenCount struct {
	Type string `json:"type"`
	Info *struct {
		Total *counts `json:"total_token_usage"`
		Last  *counts `json:"last_token_usage"`
	} `json:"info"`
	RateLimits *rateLimits `json:"rate_limits"`
}

type rateLimits struct {
	ID        string  `json:"limit_id"`
	Name      string  `json:"limit_name"`
	Plan      string  `json:"plan_type"`
	Primary   *window `json:"primary"`
	Secondary *window `json:"secondary"`
}

type window struct {
	UsedPercent float64 `json:"used_percent"`
	Minutes     int     `json:"window_minutes"`
	ResetsAt    int64   `json:"resets_at"`
}

var (
	markMeta    = []byte(`"session_meta"`)
	markContext = []byte(`"turn_context"`)
	markRecord  = []byte(`"token_usage_record"`)
	markCount   = []byte(`"token_count"`)
)

func (p *parser) Line(b []byte) ([]usage.Record, []usage.Quota, bool) {
	if !usage.IsObject(b) {
		return nil, nil, true
	}
	kind := ""
	switch {
	case bytes.Contains(b, markRecord):
		kind = "record"
	case bytes.Contains(b, markCount):
		kind = "count"
	case bytes.Contains(b, markMeta):
		kind = "meta"
	case bytes.Contains(b, markContext):
		kind = "context"
	default:
		return nil, nil, false
	}
	var l line
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, nil, true
	}
	// 子線程開頭重放的父線程歷史不算（本機的記錄裡沒有，防的是較舊的格式）。
	if l.Ordinal != nil && *l.Ordinal < p.s.Start && l.Type != "session_meta" {
		return nil, nil, false
	}
	switch {
	case l.Type == "session_meta" && kind == "meta":
		if p.s.Thread != "" {
			return nil, nil, false // 子線程檔案裡的第二個是父線程的，不用
		}
		var m meta
		if json.Unmarshal(l.Payload, &m) != nil {
			return nil, nil, true
		}
		p.s.Thread, p.s.Parent, p.s.Cwd = m.ID, m.Parent, m.Cwd
		if m.Start != nil {
			p.s.Start = *m.Start
		}
	case l.Type == "turn_context" && kind == "context":
		var c turnContext
		if json.Unmarshal(l.Payload, &c) != nil {
			return nil, nil, true
		}
		p.s.Model = cmp.Or(c.Model, p.s.Model)
		p.s.Cwd = cmp.Or(c.Cwd, p.s.Cwd)
	case l.Type == "token_usage_record":
		return p.record(l)
	case l.Type == "event_msg" && kind == "count":
		return p.count(l)
	}
	return nil, nil, false
}

// record 是一次模型回應的用量（0.153 起；也含 token_count 不算的壓縮請求）。
func (p *parser) record(l line) ([]usage.Record, []usage.Quota, bool) {
	var r record
	if json.Unmarshal(l.Payload, &r) != nil || r.Usage == nil || r.Response == "" {
		return nil, nil, true
	}
	p.s.Records = true
	t, ok := r.Usage.tokens()
	if !ok {
		return nil, nil, true
	}
	return []usage.Record{p.rec("r:"+r.Response, l.Timestamp, cmp.Or(r.Thread, p.s.Thread), t)}, nil, false
}

// count 讀額度快照；還沒見過 token_usage_record 時，也按線程累計數的增長算用量。
func (p *parser) count(l line) ([]usage.Record, []usage.Quota, bool) {
	var c tokenCount
	if json.Unmarshal(l.Payload, &c) != nil {
		return nil, nil, true
	}
	if c.Type != "token_count" {
		return nil, nil, false
	}
	var quotas []usage.Quota
	if q, ok := quota(c.RateLimits, l.Timestamp); ok {
		quotas = []usage.Quota{q}
	}
	if p.s.Records || c.Info == nil || c.Info.Total == nil {
		return nil, quotas, false
	}
	total := *c.Info.Total
	var used counts
	// 線程累計數增長了才是一次新的呼叫（只有額度變化的不算，openai/codex#14489）。
	// 用量取這次呼叫自己的數字（last_token_usage）：本機的 25 萬次呼叫裡它與累計數
	// 的增長只有 73 次不同，那幾次的增長本身不合理（緩存比輸入多、輸出是負的）。
	// 它全是 0 時才用累計數的增長。檔案裡的第一個沒有上一個累計可比，全是 0 的
	// （接續的檔案開頭只帶累計）不算。
	switch prev := p.s.Total; {
	case prev == nil:
		if c.Info.Last != nil {
			used = *c.Info.Last
		}
	case total.Total > prev.Total:
		if last := c.Info.Last; last != nil && (last.Input != 0 || last.Output != 0) {
			used = *last
		} else {
			used = total.minus(*prev)
		}
	}
	if p.s.Total == nil || total.Total >= p.s.Total.Total {
		p.s.Total = &total
	}
	if used.Input == 0 && used.Output == 0 {
		return nil, quotas, false
	}
	t, ok := used.tokens()
	if !ok {
		return nil, quotas, true
	}
	key := "t:" + p.s.Thread + ":" + strconv.FormatInt(total.Total, 10)
	return []usage.Record{p.rec(key, l.Timestamp, p.s.Thread, t)}, quotas, false
}

func (p *parser) rec(key string, at time.Time, thread string, t usage.Tokens) usage.Record {
	return usage.Record{Key: key, At: at, Session: thread, Parent: p.s.Parent, Model: p.s.Model,
		Dir: p.s.Cwd, Tokens: t}
}

// quota 是額度快照：每個額度桶（limit_id）一份，主、次兩個窗口。
func quota(r *rateLimits, at time.Time) (usage.Quota, bool) {
	if r == nil {
		return usage.Quota{}, false
	}
	q := usage.Quota{Bucket: cmp.Or(r.ID, "codex"), Name: r.Name, Plan: r.Plan, At: at}
	for _, w := range []*window{r.Primary, r.Secondary} {
		if w == nil || w.Minutes <= 0 {
			continue
		}
		lim := usage.Limit{Minutes: w.Minutes, UsedPercent: w.UsedPercent}
		if w.ResetsAt > 0 {
			lim.ResetsAt = time.Unix(w.ResetsAt, 0).UTC()
		}
		q.Windows = append(q.Windows, lim)
	}
	return q, len(q.Windows) > 0
}
