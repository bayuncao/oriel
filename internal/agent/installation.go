package agent

import (
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// Installation 是 discovery 在此機器上為一個 Agent 找到的結果。
type Installation struct {
	Agent        task.AgentID `json:"agent"`
	Availability Availability `json:"availability"`
	Auth         AuthState    `json:"auth"`
	Path         string       `json:"path,omitzero"` // 所選候選的 Exec
	PathSource   PathSource   `json:"pathSource,omitzero"`
	Version      string       `json:"version,omitzero"`
	Candidates   []Candidate  `json:"candidates"`       // 找到的每個可執行檔，依查找順序
	Problem      Problem      `json:"problem,omitzero"` // 損壞或過時的原因
	ProbedAt     time.Time    `json:"probedAt"`
}

type Availability string

const (
	Missing  Availability = "missing"
	Broken   Availability = "broken" // 找到了但無法執行，或探測失敗
	Outdated Availability = "outdated"
	Ready    Availability = "ready"
)

type AuthState string

const (
	Authenticated   AuthState = "authenticated"
	Unauthenticated AuthState = "unauthenticated"
	AuthUnknown     AuthState = "unknown"
)

// Candidate 是為偵測命令找到的一個可執行檔。它們依序被
// 檢查，第一個未被 Rejected 的會被選用；之後的候選
// 未經檢查，沒有 Exec 與 Real。
type Candidate struct {
	Path string `json:"path"` // 找到它的位置
	// Exec 是 OrielHub 執行的對象：Path，或對版本管理器的
	// 包裝腳本、shim（mise、asdf）而言是管理器指名的程式，
	// 因為包裝器可能遞迴呼叫自己。
	Exec string `json:"exec,omitzero"`
	// Real 是 Exec 解析符號連結後的結果。它用來識別 npm 或
	// Homebrew 套件，但絕不會被啟動：更新會移動它
	// （Caskroom/claude-code/<version>/、codex releases/<version>/）。
	Real     string     `json:"real,omitzero"`
	Source   PathSource `json:"source"`
	Version  string     `json:"version,omitzero"`
	Rejected Problem    `json:"rejected,omitzero"` // 為何它不是該 Agent，例如版本輸出不符合
}

// PathSource 是候選被找到的位置，依查找順序排列。
type PathSource string

const (
	FromOverride  PathSource = "override"   // 使用者明確指定的路徑；絕不退回
	FromPath      PathSource = "path"       // 登入 shell 的 PATH 快照
	FromToolDir   PathSource = "tool_dir"   // 使用者工具目錄（npm prefix、nvm、mise、bun、Homebrew……）
	FromVendorDir PathSource = "vendor_dir" // DetectSpec.Dirs
)

// Override 是使用者對各 Agent 的 discovery 設定。
type Override struct {
	Agent      task.AgentID `json:"agent"`
	Executable string       `json:"executable,omitzero"`
}
