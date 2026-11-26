package agent

import (
	"slices"

	"github.com/bayuncao/oriel/internal/task"
)

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
	// Native 是 agent 有沒有自己的寬鬆模式（Launch.Native）：沒有時 Profile 不能選它（PO07）。
	Native bool `json:"native"`
	// Override 是使用者設的覆蓋路徑（AC40）；沒設時是 ""。
	Override string `json:"override,omitzero"`
	// Pinned 是加入的 Registry 條目釘選的版本（AC20）；其他來源是 ""。
	Pinned string `json:"pinned,omitzero"`
	// Custom 是自訂 agent 的設定，供頁面編輯（AC36）；密鑰環境變數只有鑰匙串的鍵，沒有值（AC35）。
	Custom *CustomAgent `json:"custom,omitzero"`
	// Profiles 是它的 Profile，預設的在最前（Config.ProfilesOf，PO30）；遠程的 Agents.List 不給。
	Profiles []Profile `json:"profiles,omitzero"`
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
			Protocol: d.Protocol, Install: d.Install, Installation: byAgent[d.ID], Launchable: d.Launchable,
			Native: d.Launch.Native != nil}
	}
	return rows
}

// Annotate 在 rows 上接上使用者的設定：覆蓋路徑、Registry 條目的版本、自訂 agent 的設定、Profile（桌面的
// Agent 頁用；遠程只用 Rows）。
func (c Config) Annotate(rows []AgentRow) []AgentRow {
	pinned := make(map[task.AgentID]string, len(c.Registry))
	for _, a := range c.Registry {
		pinned[registryID(a.ID)] = a.Version
	}
	custom := make(map[task.AgentID]CustomAgent, len(c.Custom))
	for _, a := range c.Custom {
		custom[customID(a.Slug)] = a
	}
	for i := range rows {
		r := &rows[i]
		r.Override = c.Override(r.ID)
		r.Pinned = pinned[r.ID]
		r.Profiles = c.ProfilesOf(r.ID)
		if a, ok := custom[r.ID]; ok {
			// 列是交出去的複本：不與設定共用切片。
			a.Args, a.Env = slices.Clone(a.Args), slices.Clone(a.Env)
			r.Custom = &a
		}
	}
	return rows
}
