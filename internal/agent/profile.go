package agent

import (
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// Profile 是一個 Agent 的預設組合。每個 Agent 都有一個
// 無法刪除的預設 Profile。Profile 是設定；Session 建立時
// 會保存由它解析出的 task.LaunchSnapshot。
type Profile struct {
	ID             task.ProfileID    `json:"id"`
	Name           string            `json:"name"`
	Agent          task.AgentID      `json:"agent"`
	Default        bool              `json:"default"`
	Model          string            `json:"model,omitzero"`
	Mode           string            `json:"mode,omitzero"` // Definition.Modes 之一
	Approval       task.ApprovalMode `json:"approval"`
	ExtraArgs      []string          `json:"extraArgs"`
	Env            []task.EnvVar     `json:"env"`        // 秘密值僅以參照保存
	AskTimeout     time.Duration     `json:"askTimeout"` // 0 = 永不（預設）
	TimeoutVerdict task.Verdict      `json:"timeoutVerdict,omitzero"`
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
