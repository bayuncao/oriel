package agent

import (
	"slices"

	"github.com/bayuncao/oriel/internal/task"
)

// DefaultProfileID 是每個 Agent 自動擁有的預設 Profile 的 ID。
const DefaultProfileID task.ProfileID = "default"

// DefaultProfile 是 Agent 的預設 Profile（P01、D95）：審批經 OrielHub、
// 沒有額外參數與環境變數、不指定模型與模式、詢問永不逾時。M2 不儲存、
// 不能編輯，每次由 Definition 合成。名稱給日誌與事件看，介面依 ID 翻譯。
func DefaultProfile(def Definition) Profile {
	return Profile{ID: DefaultProfileID, Name: "Default", Agent: def.ID, Default: true,
		Approval: task.ApprovalOrielHub}
}

// Launch 把 Definition、Installation 與 Profile 解析為 Session 啟動時保存的
// Launch Snapshot（P02）：執行 Installation 選定的可執行檔；參數取 Definition
// 的，Profile 選了 agent 自己的寬鬆模式且 Definition 有對應參數時換成它，
// 再接上 Profile 的額外參數；環境變數是 Definition 的加上 Profile 的。
// caps 由呼叫者探測（D98）。純函式：不檢查 Installation 是否可用（見 Unready）。
func Launch(def Definition, inst Installation, p Profile, caps task.Capabilities) task.LaunchSnapshot {
	args := def.Launch.Args
	if p.Approval == task.ApprovalNative && def.Launch.Native != nil {
		args = def.Launch.Native
	}
	return task.LaunchSnapshot{
		Agent: def.ID, AgentName: def.Name, Protocol: def.Protocol,
		Profile: p.ID, ProfileName: p.Name,
		Executable: inst.Path,
		Args:       append(slices.Clone(args), p.ExtraArgs...),
		Env:        append(slices.Clone(def.Launch.Env), p.Env...),
		Model:      p.Model, Mode: p.Mode, Approval: p.Approval,
		AskTimeout: p.AskTimeout, TimeoutVerdict: p.TimeoutVerdict,
		Caps: caps,
	}
}

// Unready 回報 Installation 為何不能啟動（P03）：就緒時為 nil；壞了或過時時
// 是它的 Problem；沒找到時是 agent.not_installed。
func (i Installation) Unready() error {
	switch {
	case i.Availability == Ready:
		return nil
	case i.Problem.Code != "":
		p := i.Problem
		return &p
	}
	p := problem(CodeNotInstalled)
	return &p
}
