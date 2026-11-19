// Package proc 為 agent discovery 與執行期轉接器執行外部程式。
//
//   - 擷取使用者 shell 環境的快照，因為從 Finder 啟動的 app
//     不會繼承 PATH；列出使用者工具目錄；並將版本管理器的
//     包裝器與 shim（mise、asdf）解析為它們實際執行的程式。
//     規則與限制見 docs/architecture/agent.md（「發現」）。
//   - 將快照明確傳給子行程；絕不修改 os.Environ。
//   - Runner 在獨立的 session 與行程群組中以逾時執行探測（shell、
//     版本管理器、版本、認證）。逾時、取消或 Close 時終止整個群組
//     （SIGTERM、寬限期、SIGKILL），不讓任何子行程成為孤兒；保留
//     stdout 的開頭與 stderr 的結尾各 1 MiB。
//   - Runner.Start 啟動常駐的 agent 行程（Process），同樣在獨立的群組：
//     stdin、stdout 是協議管道，stderr 只留最後 8 KiB（崩潰時附在 exited
//     事實中），結束原因是 proc.exited 或 proc.signaled。行程自己結束時
//     群組裡的子孫一起結束；Stop 與 Runner.Close 依同樣的順序結束群組。
//
// 失敗一律是 *Error：穩定代碼加參數（docs/architecture/README.md 規則 9）。
package proc
