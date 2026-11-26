package agent

import (
	"encoding/base64"

	"github.com/bayuncao/oriel/internal/task"
)

// Definition 是說明如何偵測與啟動一個 Agent 的資料。
type Definition struct {
	ID   task.AgentID `json:"id"`
	Name string       `json:"name"`
	// Icon 是產品圖示的 URL：16×16 的單色 SVG，只以 currentColor 著色
	// （ACP Registry 對圖示的規定），頁面依主題上色。builtin 的圖示
	// 內嵌在二進位檔裡（IconURL）；空字串表示沒有，頁面顯示名稱的首字母。
	Icon     string            `json:"icon,omitzero"`
	Source   Source            `json:"source"`
	Protocol task.Protocol     `json:"protocol"`
	Detect   DetectSpec        `json:"detect"`
	Launch   LaunchSpec        `json:"launch"`
	Version  *VersionProbe     `json:"version"` // nil：版本未知，永不過時
	Auth     *AuthProbe        `json:"auth"`    // nil：認證狀態永遠未知
	Install  InstallHint       `json:"install"`
	Caps     task.Capabilities `json:"caps"`
	// Launchable 是能不能用它建 Task、啟動 Session：hub 有它協議的轉接器（Registry 條目與自訂 agent 是 ACP）。由 app 組目錄時
	// 填（m3.md CX53）；頁面的 Agent 欄位只列這些，不自己判斷協議。
	Launchable bool `json:"launchable"`
	// Supersedes 是此 builtin 已原生支援的 ACP Registry 條目 ID
	// （claude 對應的 claude-acp）；ListRegistry 會隱藏它們。
	Supersedes []string `json:"supersedes,omitzero"`
}

// IconURL 把內嵌的 SVG 轉成 Definition.Icon 用的 data URL。
func IconURL(svg []byte) string {
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(svg)
}

// Source 是 Definition 的來源。ID 衝突時 builtin 優先於
// registry，registry 優先於 custom。
type Source string

const (
	SourceBuiltin  Source = "builtin"  // 由執行期轉接器匯出、由 app 註冊
	SourceRegistry Source = "registry" // 使用者加入的 ACP Registry 條目
	SourceCustom   Source = "custom"   // 使用者定義的 ACP agent
)

// DetectSpec 說明哪個命令能證明 Agent 已安裝。對 npx/uvx 條目
// 而言它不同於啟動命令——偵測的是 npx 或 uvx。
//
// 候選項目只有在證明自己是此 Agent 時才算數：其真實路徑
// 位於其中一個 NPM 套件內，或其執行 Definition.Version 的輸出
// 符合 VersionProbe 的 Pattern。被拒絕的候選會連同原因保留下來，
// 然後嘗試下一個；但使用者覆寫的候選若被拒絕則是錯誤。
// 既無 NPM 也無 Version 的 Definition（自訂 agent、Registry
// npx/uvx 條目——其套件已釘選在 LaunchSpec 中）接受找到的第一個
// 可執行檔。
type DetectSpec struct {
	Command string `json:"command"`
	// Dirs 是廠商的安裝目錄，在 PATH 與使用者工具目錄之後嘗試；
	// ~ 與 $VAR 從環境快照展開，空結果會被跳過。
	Dirs []string `json:"dirs"`
	// NPM 是提供 Command 的 npm 套件，目前的名稱在前；版本從其
	// package.json 讀取，不需執行任何東西。
	NPM []string `json:"npm"`
	// Brew 是提供 Command 的 Homebrew formula 與 cask。
	Brew []string `json:"brew"`
	// NameShared 表示其他工具使用相同的命令名稱（Homebrew 的
	// grok 是 regex 工具，不是 xAI 的 Grok）：來自 PATH 或
	// 使用者工具目錄的候選還必須解析到其中一個 Brew
	// 套件；Dirs 候選只需通過身分檢查。
	NameShared bool `json:"nameShared"`
}

// LaunchSpec 是啟動 Agent 的命令列，相對於偵測到的可執行檔。
type LaunchSpec struct {
	Args []string      `json:"args"` // 例如接在 npx 之後的 ["-y", "pkg@1.2.3"]
	Env  []task.EnvVar `json:"env"`
	// Native 在 ApprovalNative（agent 自身的寬鬆模式）下取代 Args，
	// 因為其旗標可能必須放在子命令之前（grok agent
	// --always-approve stdio）；不支援時、或轉接器透過其協議
	// 選擇模式時為空。
	Native []string `json:"native"`
}

// VersionProbe 執行可執行檔以讀取其版本。
type VersionProbe struct {
	Args    []string `json:"args"`         // 例如 ["--version"]
	Pattern string   `json:"pattern"`      // 第一個擷取群組為版本的正規表示式
	Min     string   `json:"min,omitzero"` // 比它舊即為過時
	Exclude []string `json:"exclude"`      // 已知有問題的版本，視為過時
}

// BareVersion 是只印出版本號（可帶預發布後綴）的輸出所用的
// Pattern；擷取的版本不含後綴。
const BareVersion = `^(\d+\.\d+\.\d+)(?:-[0-9A-Za-z.]+)?(?:\n|$)`

// AuthProbe 執行 agent 官方的狀態命令。
type AuthProbe struct {
	Args            []string `json:"args"` // 例如 ["auth", "status"]
	AuthenticatedRE string   `json:"authenticatedRe,omitzero"`
	// 沒有 pattern 時，結束碼 0 表示已認證，其他碼表示未認證。
}

// InstallHint 在 Agent 缺失時顯示。OrielHub 從不執行它。
type InstallHint struct {
	DocsURL  string            `json:"docsUrl"`
	Commands map[string]string `json:"commands"` // 依 GOOS 區分
}
