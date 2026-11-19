package provider

import (
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// Health 是一家現在的狀態，或一天裡最差的狀態。
type Health string

const (
	HealthOperational Health = "operational"
	HealthDegraded    Health = "degraded"
	HealthOutage      Health = "outage"
)

// worse 回傳兩者中較差的；空字串最好。
func worse(a, b Health) Health {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func rank(h Health) int {
	switch h {
	case HealthOperational:
		return 1
	case HealthDegraded:
		return 2
	case HealthOutage:
		return 3
	}
	return 0
}

// ProviderDay 是 30 天色條的一格。
type ProviderDay struct {
	// Date 是本機時區的日曆日 "YYYY-MM-DD"。
	Date string `json:"date"`
	// Uptime 是這一天正常的百分比（0–100）；nil 是沒有數據（PS13、PS16、PS17）。
	Uptime *float64 `json:"uptime"`
	// Health 是這一天最差的狀態；沒有數據時為空。
	Health Health `json:"health,omitzero"`
}

// IncidentStage 是事故的一次更新所處的階段。
type IncidentStage string

const (
	StageInvestigating IncidentStage = "investigating"
	StageIdentified    IncidentStage = "identified"
	StageMonitoring    IncidentStage = "monitoring"
	StageResolved      IncidentStage = "resolved"
)

// IncidentUpdate 是事故的一次更新，原文照抄。
type IncidentUpdate struct {
	Stage   IncidentStage `json:"stage"`
	At      time.Time     `json:"at"`
	Message string        `json:"message"`
}

// ProviderIncident 是一行展開後的事故（PS07）：沒解決的，或 24 小時內解決的。
type ProviderIncident struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// URL 是這次事故在狀態頁上的頁面。
	URL string `json:"url"`
	// Updates 新的在前。
	Updates []IncidentUpdate `json:"updates"`
}

// ProviderRow 是卡片上的一行。
type ProviderRow struct {
	ID    string      `json:"id"`
	Name  string      `json:"name"`
	Group VendorGroup `json:"group"`
	Page  string      `json:"page"`
	// Components 是算進當前狀態的組件；空是整個狀態頁。
	Components []string `json:"components"`
	// WholePage：歷史按整個狀態頁的事故算，因為事故沒寫組件（PS12）。
	WholePage bool              `json:"wholePage"`
	Health    Health            `json:"health"`
	Days      []ProviderDay     `json:"days"`
	Incident  *ProviderIncident `json:"incident,omitzero"`
	ReadAt    time.Time         `json:"readAt"`
	// Problem 不為空時，這一次沒讀成，顯示的是 ReadAt 那次的結果（PS23）。
	Problem *task.Problem `json:"problem,omitzero"`
}

// UnreadVendor 是還沒讀到過、這一次也沒讀成的廠商（PS24）：不畫一行，原因只記在日誌裡；一家都沒讀到時卡片頭部寫錯誤（PS25）。
type UnreadVendor struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Page    string       `json:"page"`
	Problem task.Problem `json:"problem"`
}

// Board 是卡片要的全部內容。
type Board struct {
	// Rows 依清單順序（PS01）。
	Rows   []ProviderRow  `json:"rows"`
	Unread []UnreadVendor `json:"unread"`
	// Reading：正在讀。
	Reading bool `json:"reading"`
	// ReadAt 是上一次讀完的時間；還沒讀完過時為零值。
	ReadAt time.Time `json:"readAt,omitzero"`
	// Rev 每次變化加一：同一份內容經推送與呼叫結果兩條路到頁面時，取 Rev 大的。
	Rev uint64 `json:"rev"`
}
