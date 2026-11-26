package pi

import (
	"cmp"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bayuncao/oriel/internal/task"
)

// maxToolOutput 是一次工具輸出保留的文字（PI15）；輸出會寫進事件。
const maxToolOutput = 64 << 10

// modelOption 是模型設置的 ID：pi 只有這一個設置，以 set_model 切換。
const modelOption = "model"

// permissionTitle 是 OrielHub 的審批擴充發出的 confirm 的標題（PI31）。
const permissionTitle = "orielhub.permission"

// model 是 pi 的 Model 裡 OrielHub 讀的欄位（PI06）。
type model struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Name     string `json:"name"`
}

// value 是模型設置裡的值 provider/id；沒選模型時 pi 報 unknown 的佔位，回空。
func (m model) value() string {
	if m.Provider == "" || m.ID == "" || m.Provider == "unknown" {
		return ""
	}
	return m.Provider + "/" + m.ID
}

// tokens 是 pi 訊息的用量；input 本就不含快取（PI16）。
type tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Cost       struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// content 是工具結果的一個內容區塊。
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// event 處理 pi 的事件（PI13–PI19）。一輪之外的訊息與工具不報。
func (c *conn) event(typ string, line []byte) error {
	switch typ {
	case "extension_error":
		var m struct {
			Path  string `json:"extensionPath"`
			Event string `json:"event"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return err
		}
		if c.ext != "" && m.Path == c.ext { // PI35：pi 把出錯的 tool_call 當拦下
			c.logger.Warn("pi: the OrielHub approvals extension failed", "event", m.Event)
		}
		return nil
	case "message_update", "message_end", "tool_execution_start", "tool_execution_update", "tool_execution_end",
		"auto_retry_end", "agent_settled":
	default:
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turn {
		return nil
	}
	switch typ {
	case "message_update":
		var m struct {
			Event struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
			} `json:"assistantMessageEvent"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return err
		}
		switch m.Event.Type {
		case "text_delta":
			c.output(task.ChannelText, m.Event.Delta)
		case "thinking_delta":
			c.output(task.ChannelThinking, m.Event.Delta)
		}
	case "message_end":
		var m struct {
			Message struct {
				Role         string  `json:"role"`
				StopReason   string  `json:"stopReason"`
				ErrorMessage string  `json:"errorMessage"`
				Provider     string  `json:"provider"`
				Model        string  `json:"model"`
				Usage        *tokens `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return err
		}
		msg := m.Message
		switch msg.Role {
		case "assistant":
			c.lastStop, c.lastError = msg.StopReason, msg.ErrorMessage
			c.reportModel(model{Provider: msg.Provider, ID: msg.Model}.value())
		case "toolResult":
		default:
			return nil
		}
		if u := msg.Usage; u != nil {
			c.used.InputTokens += u.Input
			c.used.OutputTokens += u.Output
			c.used.CacheReadTokens += u.CacheRead
			c.used.CacheWriteTokens += u.CacheWrite
			c.used.CostUSD += u.Cost.Total
		}
	case "tool_execution_start":
		var m struct {
			ID   string          `json:"toolCallId"`
			Name string          `json:"toolName"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return err
		}
		if m.ID != "" && c.tools[m.ID] == nil {
			c.startTool(m.ID, m.Name, m.Args)
		}
	case "tool_execution_update", "tool_execution_end":
		var m struct {
			ID     string `json:"toolCallId"`
			Result *struct {
				Content []content `json:"content"`
			} `json:"result"`
			IsError bool `json:"isError"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return err
		}
		st := c.tools[m.ID]
		if st == nil {
			return nil
		}
		u := task.ToolUpdate{ID: task.ToolCallID(m.ID), Status: task.ToolRunning, Kind: st.kind, Title: st.title, Paths: st.paths}
		if typ == "tool_execution_update" {
			// 進度只報一次「執行中」，不帶輸出：pi 的 partialResult 是到目前為止的整段輸出，每次都報會讓
			// 長輸出的命令把同樣的文字一再寫進事件（PI15）；輸出在結束時報。
			if st.running {
				return nil
			}
			st.running = true
		} else {
			u.Status = task.ToolCompleted
			if m.IsError {
				u.Status = task.ToolFailed
			}
			if m.Result != nil {
				u.Output = truncate(text(m.Result.Content))
			}
		}
		c.out.push(task.Command{Kind: task.FactToolUpdated, ToolUpdated: &u})
	case "auto_retry_end":
		var m struct {
			Success    bool   `json:"success"`
			FinalError string `json:"finalError"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return err
		}
		if !m.Success {
			c.retryError = m.FinalError
		}
	case "agent_settled":
		var m struct {
			Aborted bool `json:"aborted"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return err
		}
		c.settled(m.Aborted)
	}
	return nil
}

// settled 結束這一輪（PI17）：已要求中斷或 pi 報 aborted 是取消；最後一條 assistant 訊息是 error 時
// 失敗；其餘完成。呼叫者持有 c.mu。
func (c *conn) settled(aborted bool) {
	t := task.TurnEnded{Result: task.TurnOK}
	switch {
	case aborted:
		t = task.TurnEnded{Result: task.TurnCancelled}
	case c.lastStop == "error":
		t = failed(fail(CodeTurnFailed, nil, "detail",
			cmp.Or(c.retryError, c.lastError, "the model request failed")))
	}
	c.endTurn(c.outcome(t))
}

// outcome：已要求中斷的一輪不論結局都是取消（PI40）。呼叫者持有 c.mu。
func (c *conn) outcome(t task.TurnEnded) task.TurnEnded {
	if c.cancelling {
		return task.TurnEnded{Result: task.TurnCancelled}
	}
	return t
}

// endTurn 先報這一輪累計的用量（全為 0 時不報），再報結局，並清掉這一輪的狀態。呼叫者持有 c.mu。
func (c *conn) endTurn(t task.TurnEnded) {
	if c.used != (task.Usage{}) {
		u := c.used
		c.out.push(task.Command{Kind: task.FactUsage, Usage: &u})
	}
	c.turn, c.promptKey, c.cancelling, c.abortPending = false, "", false, false
	c.used, c.lastStop, c.lastError, c.retryError = task.Usage{}, "", "", ""
	c.tools, c.perms = map[string]*toolState{}, map[task.PermissionID]string{}
	c.out.push(turnEnded(t))
}

// output 報一段文字；空的不報。呼叫者持有 c.mu。
func (c *conn) output(ch task.ChannelKind, s string) {
	if s != "" {
		c.out.push(task.Command{Kind: task.FactOutput, Output: &task.Output{Channel: ch, Text: s}})
	}
}

// reportModel：assistant 報出的模型是另一個可選的模型時，換成它並報出（PI18）。呼叫者持有 c.mu。
func (c *conn) reportModel(v string) {
	if c.model == nil || v == "" || v == c.model.Current ||
		!slices.ContainsFunc(c.model.Choices, func(x task.ModelChoice) bool { return x.Value == v }) {
		return
	}
	c.model.Current = v
	c.out.push(task.Command{Kind: task.FactModelReported, ModelReported: &task.ModelReport{Setting: cloneSetting(c.model)}})
}

// startTool 記下工具呼叫並報 tool_started（PI14）。呼叫者持有 c.mu。
func (c *conn) startTool(id, name string, args json.RawMessage) *toolState {
	st := c.describe(name, args)
	c.tools[id] = st
	c.out.push(task.Command{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{
		ID: task.ToolCallID(id), Tool: name, Kind: st.kind, Title: st.title, Input: string(args), Paths: st.paths}})
	return st
}

// describe 依 pi 的內建工具分類並寫標題（PI14）；Paths 取輸入的 path，相對的按工作目錄補全。
func (c *conn) describe(name string, args json.RawMessage) *toolState {
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
	}
	_ = json.Unmarshal(args, &in) // 認不得的輸入只少了標題與路徑
	st := &toolState{name: name, kind: task.ToolOther, title: name, paths: []string{}}
	if in.Path != "" {
		p := in.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.workdir, p)
		}
		st.paths = []string{p}
	}
	switch name {
	case "bash", "powershell":
		st.kind, st.title, st.command = task.ToolExecute, cmp.Or(firstLine(in.Command), name), in.Command
	case "read":
		st.kind, st.title = task.ToolRead, "Read "+cmp.Or(in.Path, "a file")
	case "edit":
		st.kind, st.title = task.ToolEdit, "Edit "+cmp.Or(in.Path, "a file")
	case "write":
		st.kind, st.title = task.ToolEdit, "Write "+cmp.Or(in.Path, "a file")
	case "grep":
		st.kind, st.title = task.ToolSearch, "Search "+cmp.Or(in.Pattern, "files")
	case "find":
		st.kind, st.title = task.ToolSearch, "Find "+cmp.Or(in.Pattern, "files")
	case "ls":
		st.kind, st.title = task.ToolSearch, "List "+cmp.Or(in.Path, ".")
	}
	return st
}

// uiRequest 處理擴充的 UI 請求（PI31、PI33、PI34）：OrielHub 的審批擴充的 confirm 報成 asked；別的擴充
// 的對話回 cancelled，不讓 pi 一直等；不等回應的（notify、setStatus 等）不理。
func (c *conn) uiRequest(line []byte) error {
	var m struct {
		ID      string `json:"id"`
		Method  string `json:"method"`
		Title   string `json:"title"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return err
	}
	switch {
	case m.ID == "":
		return nil
	case m.Method == "confirm" && m.Title == permissionTitle && c.ext != "":
		c.ask(m.ID, m.Message)
	case m.Method == "confirm", m.Method == "select", m.Method == "input", m.Method == "editor":
		c.logger.Debug("pi: dismissed a dialog from another extension", "method", m.Method)
		c.answerUI(m.ID, "cancelled", true)
	}
	return nil
}

// ask 把審批擴充的詢問報成 asked，掛著等 Answer（PI31）；一輪之外或已要求中斷時直接回不允許（PI33）。
func (c *conn) ask(uid, message string) {
	var m struct {
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"toolName"`
		Input      json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(message), &m); err != nil || m.ToolCallID == "" {
		c.logger.Debug("pi: denied an approval request OrielHub could not read", "bytes", len(message))
		c.answerUI(uid, "confirmed", false)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turn || c.cancelling {
		c.answerUI(uid, "confirmed", false)
		return
	}
	st := c.tools[m.ToolCallID]
	if st == nil { // 沒出現過的工具呼叫先補 tool_started
		st = c.startTool(m.ToolCallID, m.ToolName, m.Input)
	}
	c.permSeq++
	pid := task.PermissionID(c.permPrefix + strconv.Itoa(c.permSeq))
	c.perms[pid] = uid
	c.out.push(task.Command{Kind: task.FactAsked, Asked: &task.Asked{ID: pid, Request: task.PermissionRequest{
		Tool: st.name, Kind: st.kind, Title: st.title, Command: st.command, Paths: slices.Clone(st.paths), Input: string(m.Input)}}})
}

// setModels 記下可用的模型（PI06），回傳啟動時的模型設置：值是 provider/id，按提供方分組；目前的值是
// get_state 的模型。沒選模型，或選的不在可用的模型裡（如設定的預設模型沒有登入）時為空：選項裡沒有的值
// 不當目前的值。沒有可用的模型時回 nil。呼叫者持有 c.mu。
func (c *conn) setModels(models []model, current string) *task.ModelSetting {
	var choices []task.ModelChoice
	for _, m := range models {
		v := m.value()
		if v == "" || slices.ContainsFunc(choices, func(x task.ModelChoice) bool { return x.Value == v }) {
			continue
		}
		choices = append(choices, task.ModelChoice{Value: v, Name: cmp.Or(m.Name, m.ID), Group: m.Provider})
	}
	if len(choices) == 0 {
		return nil
	}
	if !slices.ContainsFunc(choices, func(x task.ModelChoice) bool { return x.Value == current }) {
		current = ""
	}
	c.model = &task.ModelSetting{Option: modelOption, Current: current, Choices: choices}
	return cloneSetting(c.model)
}

func cloneSetting(s *task.ModelSetting) *task.ModelSetting {
	out := *s
	out.Choices = slices.Clone(s.Choices)
	return &out
}

// message 是 prompt 的文字（PI10–PI12）：附件以絕對路徑寫成一段文字，由 pi 用自己的 read 讀（read 也
// 讀圖片）；blob 還不支援（轉接器拿不到它的內容）。
func (c *conn) message(p task.Prompt) (string, *Error) {
	var parts []string
	if text := p.Full(); strings.TrimSpace(text) != "" {
		parts = append(parts, text)
	}
	for _, a := range p.Attachments {
		if a.Path == "" {
			return "", fail(CodeAttachmentUnsupported, nil, "name", a.Name)
		}
		path := a.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.workdir, path)
		}
		parts = append(parts, "Attached file: "+path)
	}
	return strings.Join(parts, "\n\n"), nil
}

// text 是內容區塊裡的文字，以換行相接。
func text(blocks []content) string {
	var out []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "\n")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
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
