// Package agent 是 OrielHub 可執行的 agent 目錄。
//
// 註冊將三個來源合併為一份 Definition 清單（Catalog）：
//
//	builtin   由執行期轉接器匯出、由 app 套件明確註冊的
//	          Definition（絕不透過 init）
//	registry  使用者明確加入的 ACP Registry 條目，釘選在加入時的
//	          條目版本；npx/uvx 發行版透過使用者的 node 與 uv
//	          執行，binary 發行版則以命令名稱偵測
//	custom    使用者以名稱、命令、參數與環境變數定義的 ACP agent
//
// 衝突時的優先順序為 builtin > registry > custom。OrielHub 從不
// 安裝 agent：它只使用機器上已有的程式。
// RegistryClient 擷取並快取 Registry 的索引；ListRegistry
// 將其條目提供給使用者加入，FindInstalled 標出本機已裝的條目。
//
// Discovery 將每個 Definition 轉為 Installation：收集偵測命令的
// 候選項目、解析版本管理器的包裝器、確認每個候選確實是該
// Agent 並讀取其版本，然後在存在官方狀態命令時讀取
// 其登入狀態。Discovery 保管這份表格以及啟動 agent 所用的
// 環境快照。規則見 docs/architecture/agent.md（「發現」）。
//
// 給使用者看的原因與錯誤都是 Problem：穩定代碼加參數，前端依代碼
// 翻譯（docs/architecture/README.md 規則 9）。
//
// Profile（model、mode、額外參數、參照 mygo.Secrets 的環境變數）
// 的型別也在此；Launch 把 Profile 解析為 task.LaunchSnapshot。
package agent
