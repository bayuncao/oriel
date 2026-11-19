package task

// ChannelKind 區分 agent 可見的回覆與其推理過程。
type ChannelKind string

const (
	ChannelText     ChannelKind = "text"
	ChannelThinking ChannelKind = "thinking"
)

// ToolCall 是 agent 的一次工具呼叫，已跨協議正規化。
type ToolCall struct {
	ID    ToolCallID `json:"id"`
	Tool  string     `json:"tool"`
	Kind  ToolKind   `json:"kind"`
	Title string     `json:"title"`
	Input string     `json:"input,omitzero"` // 原始工具輸入，JSON 文字
	Paths []string   `json:"paths"`
}

// ToolKind 將工具分類，讓規則與 UI 能跨 agent
// 一致地對待它們（分類遵循 ACP）。
type ToolKind string

const (
	ToolRead    ToolKind = "read"
	ToolEdit    ToolKind = "edit"
	ToolDelete  ToolKind = "delete"
	ToolMove    ToolKind = "move"
	ToolSearch  ToolKind = "search"
	ToolExecute ToolKind = "execute"
	ToolThink   ToolKind = "think"
	ToolFetch   ToolKind = "fetch"
	ToolOther   ToolKind = "other"
)

type ToolStatus string

const (
	ToolPending   ToolStatus = "pending"
	ToolRunning   ToolStatus = "running"
	ToolCompleted ToolStatus = "completed"
	ToolFailed    ToolStatus = "failed"
)

// PlanEntry 是 agent 自身計畫中的一個步驟。
type PlanEntry struct {
	Text   string     `json:"text"`
	Status PlanStatus `json:"status"`
}

type PlanStatus string

const (
	PlanPending    PlanStatus = "pending"
	PlanInProgress PlanStatus = "in_progress"
	PlanCompleted  PlanStatus = "completed"
)

// Usage 是 agent 回報的 token 與成本統計。
type Usage struct {
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUsd"`
}
