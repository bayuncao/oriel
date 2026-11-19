package task

import "time"

// Ask 是權限請求：agent 要求執行某個動作。
type Ask struct {
	ID       PermissionID      `json:"id"`
	Turn     TurnID            `json:"turn"`
	Request  PermissionRequest `json:"request"`
	AskedAt  time.Time         `json:"askedAt"`  // 搭配 LaunchSnapshot.AskTimeout，為計時器期限
	Decision *Decision         `json:"decision"` // nil = 尚未決定
	Answered bool              `json:"answered"` // 決定已送達 agent
}

// PermissionRequest 是 agent 的請求，已跨協議正規化。
type PermissionRequest struct {
	Tool    string   `json:"tool"` // agent 的工具名稱，例如 "Bash"
	Kind    ToolKind `json:"kind"`
	Title   string   `json:"title"`
	Command string   `json:"command,omitzero"` // shell 命令，用於 execute
	Paths   []string `json:"paths"`
	Input   string   `json:"input,omitzero"` // 原始工具輸入，JSON 文字
}

// Decision 是對 Ask 的回應。
type Decision struct {
	Verdict Verdict        `json:"verdict"`
	Source  DecisionSource `json:"source"`
	Rule    RuleID         `json:"rule,omitzero"` // Source 為 rule 時符合的規則
	Origin  Origin         `json:"origin,omitzero"`
}

type Verdict string

const (
	Allow Verdict = "allow"
	Deny  Verdict = "deny"
)

type DecisionSource string

const (
	DecidedByUser    DecisionSource = "user"
	DecidedByRule    DecisionSource = "rule"
	DecidedByTimeout DecisionSource = "timeout"
)

// Rule 自動決定符合的 Ask。Session 與 task 範圍的規則
// 存在 Stream 中；workspace 與全域規則是由 hub 保管的設定，
// 透過 Inputs 傳給 Decide。
type Rule struct {
	ID      RuleID    `json:"id"`
	Scope   RuleScope `json:"scope"`
	ScopeID string    `json:"scopeId,omitzero"` // session、task 或 workspace ID；全域時為空
	Agent   AgentID   `json:"agent,omitzero"`   // 為空時符合所有 agent
	Match   RuleMatch `json:"match"`
	Tool    string    `json:"tool,omitzero"`
	Pattern string    `json:"pattern,omitzero"` // 命令或其前綴
	Verdict Verdict   `json:"verdict"`
}

type RuleScope string

const (
	ScopeSession   RuleScope = "session"
	ScopeTask      RuleScope = "task"
	ScopeWorkspace RuleScope = "workspace" // 「一律允許」的預設值
	ScopeGlobal    RuleScope = "global"
)

type RuleMatch string

const (
	MatchExact  RuleMatch = "exact"
	MatchPrefix RuleMatch = "prefix" // 命令的預設值
	MatchTool   RuleMatch = "tool"   // 唯讀工具的每次呼叫
)

// AskKey 是計時器掛在哪個 Ask 上的鍵：PermissionID 只在 Session 內唯一。
type AskKey struct {
	Session    SessionID
	Permission PermissionID
}
