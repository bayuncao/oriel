package codex

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bayuncao/oriel/internal/task"
)

// maxToolOutput 是一次工具輸出保留的文字（CX14）；輸出會寫進事件。
const maxToolOutput = 64 << 10

// modelOption 是模型設置的 ID：Codex 只有這一個設置，切換時在 turn/start 帶 model。
const modelOption = "model"

// model 是 model/list 回的一個模型（CX06）。
type model struct {
	ID          string `json:"id"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Hidden      bool   `json:"hidden"`
}

// tokens 是 Codex 的用量（thread/tokenUsage/updated 的 total），換算用 usage.go 的 counts。
type tokens struct {
	Input      int64 `json:"inputTokens"`
	Cached     int64 `json:"cachedInputTokens"`
	CacheWrite int64 `json:"cacheWriteInputTokens"`
	Output     int64 `json:"outputTokens"`
	Reasoning  int64 `json:"reasoningOutputTokens"`
	Total      int64 `json:"totalTokens"`
}

func (t tokens) counts() counts { return counts(t) }

// item 是 ThreadItem 裡 OrielHub 讀的欄位。
type item struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Text      string          `json:"text"`
	Summary   []string        `json:"summary"`
	Content   json.RawMessage `json:"content"`
	Command   string          `json:"command"`
	Cwd       string          `json:"cwd"`
	Actions   []action        `json:"commandActions"`
	Status    string          `json:"status"`
	Output    *string         `json:"aggregatedOutput"`
	Changes   []change        `json:"changes"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
	Query string `json:"query"`
}

// action 是 Codex 對命令的解析（commandActions）。
type action struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// change 是 fileChange 的一處修改。
type change struct {
	Path string          `json:"path"`
	Kind json.RawMessage `json:"kind"`
}

// notification 處理 Codex 的通知（CX13–CX20）。一輪之外的條目與增量（如接回會話時的殘留）不報。
func (c *conn) notification(method string, params json.RawMessage) error {
	switch method {
	case "thread/tokenUsage/updated":
		var m struct {
			Usage struct {
				Total tokens `json:"total"`
			} `json:"tokenUsage"`
		}
		if err := json.Unmarshal(params, &m); err != nil {
			return err
		}
		c.mu.Lock()
		c.total = m.Usage.Total // 一輪之外的也記下：下一輪從它算起
		c.mu.Unlock()
		return nil
	case "turn/completed":
		return c.turnCompleted(params)
	case "serverRequest/resolved":
		var m struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if err := json.Unmarshal(params, &m); err != nil {
			return err
		}
		c.mu.Lock()
		if id, ok := c.permOf[string(m.RequestID)]; ok { // Codex 清掉了它（CX33）
			delete(c.perms, id)
			delete(c.permOf, string(m.RequestID))
		}
		c.mu.Unlock()
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turn {
		return nil
	}
	switch method {
	case "turn/started":
		// 這一輪的用量從這裡算起（CX18）：讀取依序處理，它之前的累計（含接回時重放的）都已記下。
		c.start = c.total
		var m struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(params, &m); err != nil {
			return err
		}
		c.setTurnID(m.Turn.ID) // turn/start 的回應可能還沒處理
	case "item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta", "item/reasoning/summaryPartAdded":
		var m struct {
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal(params, &m); err != nil {
			return err
		}
		switch method {
		case "item/agentMessage/delta":
			c.streamed[m.ItemID] = true
			c.output(task.ChannelText, m.Delta)
		case "item/reasoning/summaryPartAdded":
			if c.streamed[m.ItemID] { // 摘要的段落之間空一行，同完成時的補報
				c.output(task.ChannelThinking, "\n\n")
			}
		case "item/reasoning/summaryTextDelta":
			c.streamed[m.ItemID], c.summarized[m.ItemID] = true, true
			c.output(task.ChannelThinking, m.Delta)
		default: // 原始推理只在沒有摘要時送，兩者不交錯
			if !c.summarized[m.ItemID] {
				c.streamed[m.ItemID] = true
				c.output(task.ChannelThinking, m.Delta)
			}
		}
	case "item/started", "item/completed":
		var m struct {
			Item item `json:"item"`
		}
		if err := json.Unmarshal(params, &m); err != nil {
			return err
		}
		c.itemEvent(m.Item, method == "item/completed")
	case "turn/plan/updated":
		var m struct {
			Plan []struct {
				Step   string `json:"step"`
				Status string `json:"status"`
			} `json:"plan"`
		}
		if err := json.Unmarshal(params, &m); err != nil {
			return err
		}
		entries := []task.PlanEntry{}
		for _, s := range m.Plan {
			if s.Step != "" {
				entries = append(entries, task.PlanEntry{Text: s.Step, Status: planStatus(s.Status)})
			}
		}
		c.out.push(task.Command{Kind: task.FactPlan, Plan: &task.PlanUpdate{Entries: entries}})
	}
	return nil
}

func planStatus(s string) task.PlanStatus {
	switch s {
	case "inProgress":
		return task.PlanInProgress
	case "completed":
		return task.PlanCompleted
	}
	return task.PlanPending
}

// output 報一段文字；空的不報。呼叫者持有 c.mu。
func (c *conn) output(ch task.ChannelKind, text string) {
	if text != "" {
		c.out.push(task.Command{Kind: task.FactOutput, Output: &task.Output{Channel: ch, Text: text}})
	}
}

// itemEvent 處理條目的開始與完成（CX13–CX16）。呼叫者持有 c.mu。
func (c *conn) itemEvent(it item, done bool) {
	switch it.Type {
	case "agentMessage":
		if done && !c.streamed[it.ID] {
			c.output(task.ChannelText, it.Text)
		}
		return
	case "reasoning":
		if done && !c.streamed[it.ID] {
			c.output(task.ChannelThinking, strings.Join(it.Summary, "\n"))
		}
		return
	case "commandExecution", "fileChange", "mcpToolCall", "webSearch":
	default:
		return
	}
	st := c.tools[it.ID]
	if st == nil {
		st = c.startTool(it)
	}
	if !done {
		return
	}
	status := task.ToolCompleted
	if it.Status == "failed" || it.Status == "declined" {
		status = task.ToolFailed
	}
	var out string
	switch {
	case it.Type == "commandExecution" && it.Output != nil:
		out = *it.Output
	case it.Type == "mcpToolCall" && it.Error != nil:
		out = it.Error.Message
	}
	c.out.push(task.Command{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{
		ID: task.ToolCallID(it.ID), Status: status, Kind: st.kind, Title: st.title, Paths: st.paths, Output: truncate(out)}})
}

// startTool 記下工具呼叫並報 tool_started（CX14–CX16）。呼叫者持有 c.mu。
func (c *conn) startTool(it item) *toolState {
	st := &toolState{}
	var input any
	switch it.Type {
	case "commandExecution":
		st.name, st.kind, st.title, st.paths = "shell", commandKind(it.Actions), firstLine(it.Command), readPaths(it.Actions)
		input = map[string]any{"command": it.Command, "cwd": it.Cwd}
	case "fileChange":
		st.name, st.kind, st.paths = "apply_patch", changeKind(it.Changes), changePaths(it.Changes)
		st.title = editTitle(st.paths)
		input = map[string]any{"paths": st.paths}
	case "mcpToolCall":
		st.name, st.kind, st.title = it.Server+"."+it.Tool, task.ToolOther, it.Server+"."+it.Tool
		input = it.Arguments
	case "webSearch":
		st.name, st.kind, st.title = "web_search", task.ToolFetch, firstNonEmpty(it.Query, "Web search")
		input = map[string]any{"query": it.Query}
	}
	c.tools[it.ID] = st
	c.out.push(task.Command{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{
		ID: task.ToolCallID(it.ID), Tool: st.name, Kind: st.kind, Title: st.title, Input: rawJSON(input), Paths: st.paths}})
	return st
}

// commandKind 照 Codex 自己對命令的解析分類（CX14）：全是讀檔是 read，全是列目錄或搜尋是 search，
// 其餘（含沒有解析的）是 execute。
func commandKind(actions []action) task.ToolKind {
	if len(actions) == 0 {
		return task.ToolExecute
	}
	read, search := true, true
	for _, a := range actions {
		read = read && a.Type == "read"
		search = search && (a.Type == "listFiles" || a.Type == "search")
	}
	switch {
	case read:
		return task.ToolRead
	case search:
		return task.ToolSearch
	}
	return task.ToolExecute
}

func readPaths(actions []action) []string {
	var out []string
	for _, a := range actions {
		if a.Type == "read" && a.Path != "" && !slices.Contains(out, a.Path) {
			out = append(out, a.Path)
		}
	}
	return out
}

// changeKind：全是刪除是 delete，其餘是 edit（CX15）。kind 是 "delete" 或帶它的物件。
func changeKind(changes []change) task.ToolKind {
	if len(changes) == 0 {
		return task.ToolEdit
	}
	for _, ch := range changes {
		if !strings.Contains(string(ch.Kind), "delete") {
			return task.ToolEdit
		}
	}
	return task.ToolDelete
}

func changePaths(changes []change) []string {
	var out []string
	for _, ch := range changes {
		if ch.Path != "" && !slices.Contains(out, ch.Path) {
			out = append(out, ch.Path)
		}
	}
	return out
}

// editTitle 是修改檔案的標題：一個檔案寫它的名字，多個加上其餘的數目。
func editTitle(paths []string) string {
	switch len(paths) {
	case 0:
		return "Edit files"
	case 1:
		return "Edit " + paths[0]
	}
	return "Edit " + paths[0] + " and " + strconv.Itoa(len(paths)-1) + " more"
}

// turnCompleted 結束這一輪（CX18、CX19）：用量是這一輪開始到現在累計的增量（D100），先報；已要求
// 中斷的一輪不論狀態都是取消（CX40）。
func (c *conn) turnCompleted(params json.RawMessage) error {
	var m struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string          `json:"message"`
				Info    json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &m); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turn || (c.turnID != "" && m.Turn.ID != c.turnID) {
		return nil
	}
	cancelling := c.cancelling
	c.turn, c.turnID, c.cancelling, c.interruptPending = false, "", false, false
	c.streamed, c.summarized, c.tools = map[string]bool{}, map[string]bool{}, map[string]*toolState{}
	c.perms, c.permOf = map[task.PermissionID]*pendingPerm{}, map[string]task.PermissionID{}
	if u, ok := usageBetween(c.start, c.total); ok {
		c.out.push(task.Command{Kind: task.FactUsage, Usage: &u})
	}
	var t task.TurnEnded
	switch {
	case cancelling, m.Turn.Status == "interrupted":
		t = task.TurnEnded{Result: task.TurnCancelled}
	case m.Turn.Status == "completed":
		t = task.TurnEnded{Result: task.TurnOK}
	default:
		msg, info := m.Turn.Status, ""
		if e := m.Turn.Error; e != nil {
			msg, info = firstNonEmpty(e.Message, msg), errorInfo(e.Info)
		}
		switch info {
		case "unauthorized":
			t = failed(fail(CodeAuthRequired, nil))
		case "usageLimitExceeded":
			t = failed(fail(CodeUsageLimit, nil))
		case "contextWindowExceeded":
			t = failed(fail(CodeContextWindow, nil))
		default:
			t = failed(fail(CodeTurnFailed, nil, "detail", msg))
		}
	}
	c.out.push(turnEnded(t))
	return nil
}

// errorInfo 是 codexErrorInfo 的名稱：字串原樣，帶資料的變體取它唯一的鍵。
func errorInfo(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		for k := range m {
			return k
		}
	}
	return ""
}

// usageBetween 是兩次累計之間的用量，換算與核對同讀本機記錄（usage.go 的 counts，D100）；沒有增加或
// 數字對不上時不報。
func usageBetween(from, to tokens) (task.Usage, bool) {
	d := to.counts().minus(from.counts())
	if d.Total <= 0 {
		return task.Usage{}, false
	}
	t, ok := d.tokens()
	if !ok {
		return task.Usage{}, false
	}
	return task.Usage{InputTokens: t.Input, CacheReadTokens: t.CacheRead, CacheWriteTokens: t.CacheWrite, OutputTokens: t.Output}, true
}

// serverRequest 處理 Codex 發起的請求（CX30、CX31、CX35、CX36）。
func (c *conn) serverRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		if err := c.ask(id, method, params); err != nil {
			c.reply(id, map[string]any{"decision": "decline"}, nil)
		}
	case "mcpServer/elicitation/request":
		c.reply(id, map[string]any{"action": "decline", "content": nil}, nil)
	case "item/permissions/requestApproval":
		c.reply(id, map[string]any{"permissions": map[string]any{}}, nil) // 省略的都算拒絕
	default:
		c.reply(id, nil, &rpcError{Code: -32601, Message: "OrielHub does not handle " + method})
	}
}

// ask 把審批請求報成 asked，掛著等 Answer（CX30、CX31）；一輪之外或已要求中斷時直接 decline（CX35）。
func (c *conn) ask(id json.RawMessage, method string, params json.RawMessage) error {
	var m struct {
		ItemID  string   `json:"itemId"`
		Command string   `json:"command"`
		Cwd     string   `json:"cwd"`
		Actions []action `json:"commandActions"`
		Reason  string   `json:"reason"`
	}
	if err := json.Unmarshal(params, &m); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.turn || c.cancelling {
		c.reply(id, map[string]any{"decision": "decline"}, nil)
		return nil
	}
	st := c.tools[m.ItemID]
	var req task.PermissionRequest
	if method == "item/commandExecution/requestApproval" {
		if st == nil && m.ItemID != "" { // 沒出現過的工具呼叫先補 tool_started
			st = c.startTool(item{Type: "commandExecution", ID: m.ItemID, Command: m.Command, Cwd: m.Cwd, Actions: m.Actions})
		}
		kind := commandKind(m.Actions)
		paths := readPaths(m.Actions)
		if st != nil && m.Command == "" { // 只帶 itemId 的請求：照條目
			kind, paths = st.kind, st.paths
		}
		cmd := m.Command
		if cmd == "" && st != nil {
			cmd = st.title
		}
		req = task.PermissionRequest{Tool: "shell", Kind: kind, Title: firstNonEmpty(m.Reason, firstLine(cmd)),
			Command: cmd, Paths: paths, Input: rawJSON(map[string]any{"command": cmd, "cwd": m.Cwd})}
	} else {
		kind, paths := task.ToolEdit, []string(nil)
		if st != nil {
			kind, paths = st.kind, st.paths
		}
		req = task.PermissionRequest{Tool: "apply_patch", Kind: kind, Title: firstNonEmpty(m.Reason, editTitle(paths)),
			Paths: paths, Input: rawJSON(map[string]any{"paths": paths})}
	}
	if req.Paths == nil {
		req.Paths = []string{}
	}
	c.permSeq++
	pid := task.PermissionID(c.permPrefix + strconv.Itoa(c.permSeq))
	c.perms[pid] = &pendingPerm{id: id}
	c.permOf[string(id)] = pid
	c.out.push(task.Command{Kind: task.FactAsked, Asked: &task.Asked{ID: pid, Request: req}})
	return nil
}

// setModels 記下 model/list 的模型（不含 hidden，CX06），回傳啟動時的模型設置；目前的值是會話回應裡
// 的 model。沒有可選的模型時回 nil。呼叫者持有 c.mu。
func (c *conn) setModels(models []model, current string) *task.ModelSetting {
	var choices []task.ModelChoice
	for _, m := range models {
		v := firstNonEmpty(m.Model, m.ID)
		if m.Hidden || v == "" {
			continue
		}
		choices = append(choices, task.ModelChoice{Value: v, Name: firstNonEmpty(m.DisplayName, v), Description: m.Description})
	}
	if len(choices) == 0 {
		return nil
	}
	if current == "" {
		current = choices[0].Value
	}
	c.model = &task.ModelSetting{Option: modelOption, Current: current, Choices: choices}
	return cloneSetting(c.model)
}

func cloneSetting(s *task.ModelSetting) *task.ModelSetting {
	out := *s
	out.Choices = slices.Clone(s.Choices)
	return &out
}

// input 是 turn/start 的 input（CX10–CX12）：文字一塊；工作目錄裡的圖片以 localImage 給 Codex，其他
// 檔案以絕對路徑寫成一段文字，由它自己讀；blob 還不支援（轉接器拿不到它的內容）。
func (c *conn) input(p task.Prompt) ([]map[string]any, *Error) {
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
		if strings.HasPrefix(a.MIME, "image/") {
			out = append(out, map[string]any{"type": "localImage", "path": path})
			continue
		}
		out = append(out, map[string]any{"type": "text", "text": "Attached file: " + path})
	}
	return out, nil
}

func rawJSON(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case json.RawMessage:
		return string(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
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

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}
