package agent

import "github.com/bayuncao/oriel/internal/task"

// AgentRow 是 agent 列表的一行：定義接上發現結果。桌面的 agent 列表與遠程的
// Agents.List 都給它。名字保留 Agent 前綴：生成的 TypeScript 只用型別名。
type AgentRow struct {
	ID   task.AgentID `json:"id"`
	Name string       `json:"name"`
	// Icon 是 Definition.Icon：產品圖示的 URL（builtin 為內嵌 SVG 的
	// data URL）；空字串表示沒有，頁面顯示名稱的首字母。
	Icon         string        `json:"icon,omitzero"`
	Source       Source        `json:"source"`
	Protocol     task.Protocol `json:"protocol"`
	Install      InstallHint   `json:"install"`
	Installation Installation  `json:"installation"`
	// Launchable 同 Definition.Launchable。
	Launchable bool `json:"launchable"`
}

// Rows 按 defs 的順序給每個定義一行，接上它的發現結果；沒有發現結果的
// Installation 是零值。
func Rows(defs []Definition, installs []Installation) []AgentRow {
	byAgent := make(map[task.AgentID]Installation, len(installs))
	for _, i := range installs {
		byAgent[i.Agent] = i
	}
	rows := make([]AgentRow, len(defs))
	for i, d := range defs {
		rows[i] = AgentRow{ID: d.ID, Name: d.Name, Icon: d.Icon, Source: d.Source,
			Protocol: d.Protocol, Install: d.Install, Installation: byAgent[d.ID], Launchable: d.Launchable}
	}
	return rows
}
