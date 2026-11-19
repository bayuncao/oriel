// Package usage 讀各 agent 在本機記下的 token 用量與額度快照，存進 OrielHub
// 的資料庫，供用量頁查詢（docs/architecture/usage.md、ADR-34–36）。
//
// 本包宣告兩個端口：Source 讀一個 agent 的本機記錄（runtime/<agent> 實作，
// 記錄格式不離開那些包，規則 3），Store 存取讀到的用量（store 實作，測試用
// MemStore）。本包只匯入 task 與 proc，拿不到 proc.Runner，所以不會執行
// 任何程式（D87）；電腦醒來的通知與本機時區由 app 注入。
package usage
