package acp

import (
	"cmp"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/bayuncao/oriel/internal/task"
)

// maxToolOutput 是一次工具輸出保留的文字（C16）；輸出會寫進事件。
const maxToolOutput = 64 << 10

// update 把一則 session/update 轉成事實（C13–C20）。
func (c *conn) update(n sdk.SessionNotification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loading || (c.sid != "" && n.SessionId != c.sid) {
		return
	}
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		c.text(task.ChannelText, u.AgentMessageChunk.Content)
	case u.AgentThoughtChunk != nil:
		c.text(task.ChannelThinking, u.AgentThoughtChunk.Content)
	case u.ToolCall != nil:
		c.toolCall(u.ToolCall)
	case u.ToolCallUpdate != nil:
		t := u.ToolCallUpdate
		st := c.ensureTool(t.ToolCallId, t.Title, t.Kind, t.RawInput, t.Locations)
		c.toolUpdated(t.ToolCallId, st, t.Status, t.Content, t.RawOutput)
	case u.Plan != nil:
		entries := make([]task.PlanEntry, 0, len(u.Plan.Entries))
		for _, e := range u.Plan.Entries {
			entries = append(entries, task.PlanEntry{Text: e.Content, Status: planStatus(e.Status)})
		}
		c.out.push(task.Command{Kind: task.FactPlan, Plan: &task.PlanUpdate{Entries: entries}})
	case u.ConfigOptionUpdate != nil && !c.bound && c.replied:
		// 握手已回應、還沒綁定：agent 啟動之後的變化，綁定時另報（C46）。
		c.afterConfig = u.ConfigOptionUpdate.ConfigOptions
	case u.ConfigOptionUpdate != nil && !c.bound:
		// 握手中的更新是 agent 啟動時的樣子，不是它之後換了（不然會被當成 V84 跟著它，丟掉使用者選的）：
		// 併進 Start 的報告（C49）。
		c.startConfig = u.ConfigOptionUpdate.ConfigOptions
	case u.ConfigOptionUpdate != nil:
		c.out.push(task.Command{Kind: task.FactModelReported,
			ModelReported: &task.ModelReport{Setting: modelOf(u.ConfigOptionUpdate.ConfigOptions)}})
	}
	// 其餘種類（指令清單、模式、會話資訊、上下文用量、使用者訊息）不報（C19）。
}

// modelOf 取出會話設置中分類為 model 的 select（C45，D118）；沒有時為 nil。分組的選項攤平，帶上組名。
func modelOf(opts []sdk.SessionConfigOption) *task.ModelSetting {
	for _, o := range opts {
		s := o.Select
		if s == nil || s.Category == nil || *s.Category != sdk.SessionConfigOptionCategoryModel {
			continue
		}
		m := &task.ModelSetting{Option: string(s.Id), Current: string(s.CurrentValue), Choices: []task.ModelChoice{}}
		add := func(group string, cs []sdk.SessionConfigSelectOption) {
			for _, c := range cs {
				m.Choices = append(m.Choices, task.ModelChoice{Value: string(c.Value), Name: c.Name,
					Description: deref(c.Description), Group: group})
			}
		}
		if s.Options.Ungrouped != nil {
			add("", *s.Options.Ungrouped)
		}
		if s.Options.Grouped != nil {
			for _, g := range *s.Options.Grouped {
				add(g.Name, g.Options)
			}
		}
		return m
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// text 報一塊文字；不是文字的內容丟掉（C14）。
func (c *conn) text(ch task.ChannelKind, b sdk.ContentBlock) {
	if b.Text == nil || b.Text.Text == "" {
		return
	}
	c.out.push(task.Command{Kind: task.FactOutput, Output: &task.Output{Channel: ch, Text: b.Text.Text}})
}

// toolCall 報一個新的工具呼叫；已經開始的就當作更新（C15）。
func (c *conn) toolCall(t *sdk.SessionUpdateToolCall) {
	_, known := c.tools[t.ToolCallId]
	kind := t.Kind
	st := c.ensureTool(t.ToolCallId, &t.Title, &kind, t.RawInput, t.Locations)
	var status *sdk.ToolCallStatus
	if t.Status != "" {
		status = &t.Status
	}
	if known || (status != nil && *status != sdk.ToolCallStatusPending) || t.Content != nil || t.RawOutput != nil {
		c.toolUpdated(t.ToolCallId, st, status, t.Content, t.RawOutput)
	}
}

// ensureTool 回傳工具呼叫的狀態，沒見過的先補一個 tool_started（C17）；
// 帶來的欄位蓋掉舊值。呼叫者持有 c.mu。
func (c *conn) ensureTool(id sdk.ToolCallId, title *string, kind *sdk.ToolKind, input any, locs []sdk.ToolCallLocation) *toolState {
	st := c.tools[id]
	if st == nil {
		st = &toolState{kind: task.ToolOther, status: task.ToolPending}
		c.tools[id] = st
		defer func() {
			c.out.push(task.Command{Kind: task.FactToolStarted, ToolStarted: &task.ToolCall{
				ID: task.ToolCallID(id), Tool: string(st.kind), Kind: st.kind, Title: st.title,
				Input: rawJSON(st.input), Paths: st.paths}})
		}()
	}
	if title != nil {
		st.title = *title
	}
	if kind != nil {
		st.kind = toolKind(*kind)
	}
	if input != nil {
		st.input = input
	}
	if locs != nil {
		st.paths = paths(locs)
	}
	return st
}

// toolUpdated 報工具呼叫的狀態與輸出，連同目前的分類、標題與路徑（更新或權限請求可能才給出它們）；沒帶的沿用上次的（C16）。
func (c *conn) toolUpdated(id sdk.ToolCallId, st *toolState, status *sdk.ToolCallStatus, content []sdk.ToolCallContent, raw any) {
	if status != nil {
		st.status = toolStatus(*status)
	}
	if out, ok := toolText(content, raw); ok {
		st.output = out
	}
	c.out.push(task.Command{Kind: task.FactToolUpdated, ToolUpdated: &task.ToolUpdate{
		ID: task.ToolCallID(id), Status: st.status, Kind: st.kind, Title: st.title, Paths: st.paths,
		Output: st.output}})
}

// toolKind 沿用 ACP 的分類；switch_mode 與不認得的記為 other（D99）。
func toolKind(k sdk.ToolKind) task.ToolKind {
	switch k {
	case sdk.ToolKindRead, sdk.ToolKindEdit, sdk.ToolKindDelete, sdk.ToolKindMove,
		sdk.ToolKindSearch, sdk.ToolKindExecute, sdk.ToolKindThink, sdk.ToolKindFetch:
		return task.ToolKind(k)
	}
	return task.ToolOther
}

func toolStatus(s sdk.ToolCallStatus) task.ToolStatus {
	switch s {
	case sdk.ToolCallStatusInProgress:
		return task.ToolRunning
	case sdk.ToolCallStatusCompleted:
		return task.ToolCompleted
	case sdk.ToolCallStatusFailed:
		return task.ToolFailed
	}
	return task.ToolPending
}

func planStatus(s sdk.PlanEntryStatus) task.PlanStatus {
	switch s {
	case sdk.PlanEntryStatusInProgress:
		return task.PlanInProgress
	case sdk.PlanEntryStatusCompleted:
		return task.PlanCompleted
	}
	return task.PlanPending
}

// toolText 是工具輸出的文字：內容裡的文字塊；沒有內容時取字串型的
// rawOutput。第二個值表示這次帶了輸出（內容是空的也算，表示清空）。
// diff 與終端不算輸出：編輯的檔案在 Paths 裡。
func toolText(content []sdk.ToolCallContent, raw any) (string, bool) {
	if content != nil {
		var parts []string
		for _, x := range content {
			if x.Content != nil && x.Content.Content.Text != nil {
				parts = append(parts, x.Content.Content.Text.Text)
			}
		}
		return truncate(strings.Join(parts, "\n")), true
	}
	if s, ok := raw.(string); ok {
		return truncate(s), true
	}
	return "", false
}

// truncate 只留開頭 64 KiB（不切斷字元），截掉時以 "\n…" 結尾。
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

func paths(locs []sdk.ToolCallLocation) []string {
	out := make([]string, 0, len(locs))
	for _, l := range locs {
		if l.Path != "" {
			out = append(out, l.Path)
		}
	}
	return out
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

// command 是 execute 類請求的命令，規則按它的前綴放行（D99）：rawInput 的
// command，字串原樣，陣列按 shell 引號拼接。
func command(kind task.ToolKind, input any) string {
	if kind != task.ToolExecute {
		return ""
	}
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	switch v := m["command"].(type) {
	case string:
		return v
	case []any:
		args := make([]string, 0, len(v))
		for _, a := range v {
			s, ok := a.(string)
			if !ok {
				return ""
			}
			args = append(args, shellQuote(s))
		}
		return strings.Join(args, " ")
	}
	return ""
}

// shellQuote 在需要時以單引號包住 s。
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("-_./=:,+@%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// usageOf 換算一輪的用量（D100）：輸入減去快取讀寫 / 快取讀 / 快取寫 /
// 輸出（加推理，若總數是這樣算的）；四類之和對不上總數就不報（U42）。
func usageOf(u *sdk.Usage) (task.Usage, bool) {
	if u == nil || u.TotalTokens <= 0 {
		return task.Usage{}, false
	}
	deref := func(p *int) int64 {
		if p == nil {
			return 0
		}
		return int64(*p)
	}
	cr, cw, thought := deref(u.CachedReadTokens), deref(u.CachedWriteTokens), deref(u.ThoughtTokens)
	in := int64(u.InputTokens) - cr - cw
	total := int64(u.TotalTokens)
	if in < 0 || cr < 0 || cw < 0 || u.OutputTokens < 0 || thought < 0 {
		return task.Usage{}, false
	}
	for _, out := range []int64{int64(u.OutputTokens) + thought, int64(u.OutputTokens)} {
		if in+cr+cw+out == total {
			return task.Usage{InputTokens: in, CacheReadTokens: cr, CacheWriteTokens: cw, OutputTokens: out}, true
		}
	}
	return task.Usage{}, false
}

// blocks 是 Prompt 的內容：文字，加上工作目錄裡的檔案作為 resource_link；
// blob 附件還不支援（C11、C12、D100）。
func (c *conn) blocks(p task.Prompt) ([]sdk.ContentBlock, *Error) {
	var out []sdk.ContentBlock
	if strings.TrimSpace(p.Text) != "" {
		out = append(out, sdk.TextBlock(p.Text))
	}
	for _, a := range p.Attachments {
		if a.Path == "" {
			return nil, fail(CodeAttachmentUnsupported, nil, "name", a.Name)
		}
		path := a.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.workdir, path)
		}
		b := sdk.ResourceLinkBlock(cmp.Or(a.Name, filepath.Base(path)), (&url.URL{Scheme: "file", Path: path}).String())
		if a.MIME != "" {
			mime := a.MIME
			b.ResourceLink.MimeType = &mime
		}
		out = append(out, b)
	}
	return out, nil
}
