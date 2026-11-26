package agent

import (
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// Profile 是一個 Agent 的啟動設定（m4.md 第 3 節，D95）。每個 Agent 都有預設 Profile（ID 為
// DefaultProfileID，可以改、不能刪、不能改名），使用者可以再建別的。Profile 是設定；Session 建立時
// 保存由它解析出的 task.LaunchSnapshot，之後改 Profile 不影響它（PO15）。
type Profile struct {
	ID       task.ProfileID    `json:"id"`
	Name     string            `json:"name"` // 預設 Profile 是 "Default"，介面依 ID 翻譯（PO16）
	Agent    task.AgentID      `json:"agent"`
	Default  bool              `json:"default"`
	Approval task.ApprovalMode `json:"approval"`
	// ExtraArgs 原樣接在 agent 的參數後面，不經 shell（PO14）。
	ExtraArgs []string `json:"extraArgs"`
	// Env 接在 agent 自己的環境變數後面，同名時這裡的生效（PO12）；密鑰只存鑰匙串的鍵（PO13）。
	Env            []task.EnvVar `json:"env"`
	AskTimeout     time.Duration `json:"askTimeout"`              // 0 = 永不（預設）
	TimeoutVerdict task.Verdict  `json:"timeoutVerdict,omitzero"` // AskTimeout 為正時必填（PO09）
}

// MaxAskTimeout 是審批超時的上限（PO10）。
const MaxAskTimeout = 24 * time.Hour

// ProfileInput 是頁面送來的 Profile（PO03、PO31、PO32）。ID 為空時新建；密鑰的值只在這裡經過，存進鑰匙串。
type ProfileInput struct {
	Agent          task.AgentID      `json:"agent"`
	ID             task.ProfileID    `json:"id"`
	Name           string            `json:"name"`
	Approval       task.ApprovalMode `json:"approval"`
	ExtraArgs      []string          `json:"extraArgs"`
	Env            []EnvInput        `json:"env"`
	AskTimeout     time.Duration     `json:"askTimeout"`
	TimeoutVerdict task.Verdict      `json:"timeoutVerdict"`
}

// CustomAgent 是使用者定義的 ACP agent；其格式與
// ACP Registry 條目相容。
type CustomAgent struct {
	Slug    string        `json:"slug"` // Agent ID 為 "custom:<slug>"
	Name    string        `json:"name"`
	Command string        `json:"command"`
	Args    []string      `json:"args"`
	Env     []task.EnvVar `json:"env"`
}

// RegistryAgent 是使用者加入的 ACP Registry 條目，釘選到特定版本。
type RegistryAgent struct {
	ID      string    `json:"id"` // Agent ID 為 "registry:<id>"
	Version string    `json:"version"`
	Entry   []byte    `json:"entry"` // 擷取時的 registry.json 條目
	AddedAt time.Time `json:"addedAt"`
}
