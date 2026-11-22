package claude

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// maxToolOutput 是一次工具輸出保留的文字（CL15）；輸出會寫進事件。
const maxToolOutput = 64 << 10

// modelOption 是模型設置的 ID：Claude 只有這一個設置，切換時用 set_model。
const modelOption = "model"

// streamEvent：主對話的文字與推理增量各是一個 output（CL13）。子 agent 的增量不進主對話（CL20）。
func (c *conn) streamEvent(line []byte) error {
	var m struct {
		Parent *string `json:"parent_tool_use_id"`
		Event  struct {
			Type    string `json:"type"`
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		} `json:"event"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	if m.Parent != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turn {
		return nil // 一輪之外的內容（如接回會話時重放的歷史）不是這個 Session 的輸出
	}
	switch e := m.Event; e.Type {
	case "message_start":
		c.streamed[e.Message.ID] = true
	case "content_block_delta":
		switch e.Delta.Type {
		case "text_delta":
			c.output(task.ChannelText, e.Delta.Text)
		case "thinking_delta":
			c.output(task.ChannelThinking, e.Delta.Thinking)
		}
	}
	return nil
}

// output 報一段文字；空的不報。呼叫者持有 c.mu。
func (c *conn) output(ch task.ChannelKind, text string) {
	if text != "" {
		c.out.push(task.Command{Kind: task.FactOutput, Output: &task.Output{Channel: ch, Text: text}})
	}
}

// assistant 是一則完整的 assistant 訊息：工具呼叫報 tool_started（CL14，子 agent 的也報，CL20），
// TodoWrite 報 plan（CL16）；文字已由 stream_event 送過就不重複報，沒送過（不支援增量的版本）才報（CL13）。
func (c *conn) assistant(line []byte) error {
	var m struct {
		Parent  *string `json:"parent_tool_use_id"`
		Message struct {
			ID      string  `json:"id"`
			Content []block `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turn {
		return nil
	}
	main := m.Parent == nil
	streamed := c.streamed[m.Message.ID]
	for _, b := range m.Message.Content {
		switch b.Type {
		case "text":
			if main && !streamed {
				c.output(task.ChannelText, b.Text)
			}
		case "thinking":
			if main && !streamed {
				c.output(task.ChannelThinking, b.Thinking)
			}
		case "tool_use":
			if _, known := c.tools[b.ID]; known || b.ID == "" {
				continue
			}
			input := decode(b.Input)
			if main && isPlanTool(b.Name) {
				c.plan(input)
				continue
			}
			c.startTool(b.ID, b.Name, input)
		}
	}
	return nil
}

// block 是訊息內容裡的一塊。
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// startTool 記下工具呼叫並報 tool_started（CL14）。呼叫者持有 c.mu。
func (c *conn) startTool(id, name string, input any) *toolState {
	st := &toolState{name: name, kind: kindOf(name), title: titleOf(name, input), input: input, paths: pathsOf(input)}
	c.tools[id] = st
	c.out.push(task.Command{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{
		ID: task.ToolCallID(id), Tool: name, Kind: st.kind, Title: st.title, Input: rawJSON(input), Paths: st.paths}})
	return st
}

// plan 把 TodoWrite 的待辦整份報成 plan（CL16）。呼叫者持有 c.mu。
func (c *conn) plan(input any) {
	m, _ := input.(map[string]any)
	todos, _ := m["todos"].([]any)
	entries := []task.PlanEntry{}
	for _, t := range todos {
		td, _ := t.(map[string]any)
		text, _ := td["content"].(string)
		status, _ := td["status"].(string)
		if text == "" {
			continue
		}
		entries = append(entries, task.PlanEntry{Text: text, Status: planStatus(status)})
	}
	c.out.push(task.Command{Kind: task.FactPlan, Plan: &task.PlanUpdate{Entries: entries}})
}

func planStatus(s string) task.PlanStatus {
	switch s {
	case "in_progress":
		return task.PlanInProgress
	case "completed":
		return task.PlanCompleted
	}
	return task.PlanPending
}

// user 帶著工具的結果：報 tool_updated（CL15）。OrielHub 自己送的 user 訊息 Claude 不回顯。
func (c *conn) user(line []byte) error {
	var m struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	var blocks []block
	if json.Unmarshal(m.Message.Content, &blocks) != nil {
		return nil // 純文字的內容：不是工具的結果
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turn {
		return nil
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		st := c.tools[b.ToolUseID]
		if st == nil {
			continue // TodoWrite（已報成 plan）或沒見過的工具
		}
		status := task.ToolCompleted
		if b.IsError {
			status = task.ToolFailed
		}
		c.out.push(task.Command{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{
			ID: task.ToolCallID(b.ToolUseID), Status: status, Kind: st.kind, Title: st.title, Paths: st.paths,
			Output: truncate(resultText(b.Content))}})
	}
	return nil
}

// resultText 是工具結果裡的文字：字串原樣，區塊取 text 的那些（圖片不算）。
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// result 結束這一輪：有用量先報 usage（D100，CL18），再報結局（CL18、CL19）；已要求中斷的一輪
// 不論 subtype 都是取消（CL40）。沒有進行中的一輪時（行程啟動時接回的殘留）不報。
func (c *conn) result(line []byte) error {
	var m struct {
		Subtype string   `json:"subtype"`
		IsError bool     `json:"is_error"`
		Result  string   `json:"result"`
		Errors  []string `json:"errors"`
		Usage   *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turn {
		return nil
	}
	cancelling := c.cancelling
	c.turn, c.cancelling = false, false
	c.streamed = map[string]bool{}
	c.tools = map[string]*toolState{} // 一輪之外不處理工具，記著的只會越積越多
	c.perms, c.permOf = map[task.PermissionID]*pendingPerm{}, map[string]task.PermissionID{}
	called := false // 這一輪確實呼叫過模型
	if u := m.Usage; u != nil && u.Input >= 0 && u.Output >= 0 && u.CacheRead >= 0 && u.CacheWrite >= 0 &&
		u.Input+u.Output+u.CacheRead+u.CacheWrite > 0 {
		called = true
		c.out.push(task.Command{Kind: task.FactUsage, Usage: &task.Usage{InputTokens: u.Input, OutputTokens: u.Output,
			CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite}})
	}
	detail := firstNonEmpty(strings.Join(m.Errors, "; "), m.Result, m.Subtype)
	var t task.TurnEnded
	switch {
	case cancelling:
		t = task.TurnEnded{Result: task.TurnCancelled}
	case m.Subtype == "success" && !m.IsError:
		t = task.TurnEnded{Result: task.TurnOK}
	case m.Subtype == "error_max_turns":
		t = failed(fail(CodeMaxTurns, nil))
	case m.Subtype == "error_during_execution":
		t = failed(fail(CodeExecutionError, nil, "detail", detail))
	default:
		t = failed(fail(CodeTurnFailed, nil, "detail", detail))
	}
	c.out.push(turnEnded(t))
	if t.Result == task.TurnOK && called {
		c.confirmQuota()
	}
	return nil
}

// confirmQuota 在呼叫過模型而成功結束的一輪之後，把這個行程最近報來的額度以現在的時間再報一次
// （CQ14）：Claude 只在窗口的取整百分比、重置時間、狀態或門檻變了才發 rate_limit_event，這一輪
// 沒發就表示數字到現在都沒變，截至何時應是現在。只在讀取的 goroutine 上呼叫。
func (c *conn) confirmQuota() {
	if c.lastQuota == nil {
		return
	}
	q := *c.lastQuota
	q.Windows = slices.Clone(q.Windows)
	q.At = time.Now().UTC()
	c.quota(q)
}

// system 只讀 init：它報出這一輪實際用的模型（解析後的名稱），對上可選模型之一而與目前的
// 不同時報 model_reported（設計見 docs/acceptance/m3.md CL06）。
func (c *conn) system(line []byte) error {
	var m struct {
		Subtype string `json:"subtype"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	if m.Subtype != "init" || m.Model == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turn || c.model == nil || c.resolves(c.model.Current, m.Model) {
		return nil
	}
	for _, ch := range c.choices {
		if ch.resolved == m.Model || ch.value == m.Model {
			c.model.Current = ch.value
			c.out.push(task.Command{Kind: task.FactModelReported,
				ModelReported: &task.ModelReport{Setting: cloneSetting(c.model)}})
			return nil
		}
	}
	return nil
}

// quotaWindows 是 rate_limit_event 裡要記的窗口與它們的長度（分鐘）：只要 5 小時與 7 天
// （D87）；seven_day_overage_included 與頂層說的是眼下限制著的那個窗口的欄位不讀（CQ04）。
var quotaWindows = []struct {
	key     string
	minutes int
}{{"five_hour", 300}, {"seven_day", 10080}}

// rateLimit 把 rate_limit_event 的 unifiedWindows 記成 Claude 的額度快照（D87、m3.md CQ01–CQ07）。
// 時間是收到的時間：事件不帶時間；成功的一輪結束時再以當時的時間報一次（confirmQuota）。沒有可記的窗口（API key、Bedrock、Vertex 沒有這些欄位）時
// 不記；某個窗口讀不懂就跳過它（CQ06）。不產生 hub 的事實。
func (c *conn) rateLimit(line []byte) error {
	if c.quota == nil {
		return nil
	}
	var m struct {
		Info *struct {
			Windows map[string]json.RawMessage `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	if m.Info == nil {
		return nil
	}
	q := usage.Quota{Agent: claudeAgent, Bucket: string(claudeAgent), At: time.Now().UTC()}
	for _, w := range quotaWindows {
		raw, ok := m.Info.Windows[w.key]
		if !ok {
			continue
		}
		var v struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *int64   `json:"resetsAt"`
		}
		if err := json.Unmarshal(raw, &v); err != nil || v.Utilization == nil || v.ResetsAt == nil || *v.ResetsAt <= 0 {
			c.logger.Debug("claude: skipped a quota window OrielHub could not read", "window", w.key)
			continue
		}
		q.Windows = append(q.Windows, usage.Limit{Minutes: w.minutes, UsedPercent: *v.Utilization * 100,
			ResetsAt: time.Unix(*v.ResetsAt, 0).UTC()})
	}
	if len(q.Windows) > 0 {
		c.lastQuota = &q
		c.quota(q)
	}
	return nil
}

// resolves 回報可選模型 value 是否就是 model。呼叫者持有 c.mu。
func (c *conn) resolves(value, model string) bool {
	if value == model {
		return true
	}
	for _, ch := range c.choices {
		if ch.value == value {
			return ch.resolved == model
		}
	}
	return false
}

// setModels 記下 initialize 報的可選模型（CL06），回傳啟動時的模型設置；沒有可選的模型時回 nil。
// 目前的值先是 default（Claude 自己的預設，含使用者設定裡選的模型），第一輪的 system init
// 報出實際的模型後再對上。呼叫者持有 c.mu。
func (c *conn) setModels(models []model) *task.ModelSetting {
	var choices []task.ModelChoice
	c.choices = nil
	for _, m := range models {
		if m.Value == "" {
			continue
		}
		choices = append(choices, task.ModelChoice{Value: m.Value, Name: firstNonEmpty(m.DisplayName, m.Value),
			Description: m.Description})
		c.choices = append(c.choices, choice{value: m.Value, resolved: m.Resolved})
	}
	if len(choices) == 0 {
		return nil
	}
	current := choices[0].Value
	if slices.ContainsFunc(choices, func(ch task.ModelChoice) bool { return ch.Value == "default" }) {
		current = "default"
	}
	c.model = &task.ModelSetting{Option: modelOption, Current: current, Choices: choices}
	return cloneSetting(c.model)
}

func cloneSetting(s *task.ModelSetting) *task.ModelSetting {
	out := *s
	out.Choices = slices.Clone(s.Choices)
	return &out
}

// content 是 user 訊息的內容（CL10–CL12）：文字一塊；工作目錄裡的檔案以絕對路徑告訴 Claude，
// 由它用自己的工具讀；blob 還不支援（轉接器拿不到它的內容）。
func (c *conn) content(p task.Prompt) ([]map[string]any, *Error) {
	out := []map[string]any{}
	if strings.TrimSpace(p.Text) != "" {
		out = append(out, map[string]any{"type": "text", "text": p.Text})
	}
	for _, a := range p.Attachments {
		if a.Path == "" {
			return nil, fail(CodeAttachmentUnsupported, nil, "name", a.Name)
		}
		path := a.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.workdir, path)
		}
		out = append(out, map[string]any{"type": "text", "text": "Attached file: " + path})
	}
	return out, nil
}

// isPlanTool 是 Claude 的待辦工具：它的輸入是整份計畫（CL16）。
func isPlanTool(name string) bool { return name == "TodoWrite" }

// kindOf 把 Claude 的工具名稱對應到 ToolKind（CL17）。
func kindOf(name string) task.ToolKind {
	switch name {
	case "Bash":
		return task.ToolExecute
	case "Read":
		return task.ToolRead
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return task.ToolEdit
	case "Glob", "Grep":
		return task.ToolSearch
	case "WebFetch", "WebSearch":
		return task.ToolFetch
	}
	return task.ToolOther
}

// titleOf 是工具呼叫的標題：輸入裡最能說明它在做什麼的一項（Grep、Glob 的 pattern 先於它們的 path），
// 都沒有時是工具名稱。
func titleOf(name string, input any) string {
	m, _ := input.(map[string]any)
	for _, k := range []string{"description", "command", "pattern", "file_path", "notebook_path", "path", "url", "query"} {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return firstLine(s)
		}
	}
	return name
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// pathsOf 是工具輸入裡的檔案路徑（CL14）。
func pathsOf(input any) []string {
	m, _ := input.(map[string]any)
	var out []string
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if s, ok := m[k].(string); ok && s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// command 是 execute 工具的 shell 命令（CL30）。
func command(kind task.ToolKind, input any) string {
	if kind != task.ToolExecute {
		return ""
	}
	m, _ := input.(map[string]any)
	s, _ := m["command"].(string)
	return s
}

func decode(raw json.RawMessage) any {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

func rawJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// compact 是原始 JSON 去掉空白後的文字；不是 JSON 時為空。
func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if len(raw) == 0 || json.Compact(&b, raw) != nil {
		return ""
	}
	return b.String()
}

func truncate(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	const mark = "\n…"
	cut := maxToolOutput - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}
