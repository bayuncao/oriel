// Package hub 是包在 task 套件外的命令式外殼：每個活的 Task
// 一個 Actor goroutine（單一寫入者），把客戶端 Command 與執行期
// 事實經 task.Decide 處理、在同一交易中附加事件與投影、再對
// task.Plan 的輸出在 Actor 外執行 Effect。
//
// 對外介面是 CreateTask、Command、Subscribe（transport 使用）；
// port 見 ports.go（Log、Config、Runtime、Git、Notifier），另有
// 記憶體真實現 MemStore（同一物件實現 Log+Config：Append 同交易
// 寫入記錄、遮蔽、投影與新設定）。Actor 的信箱種類、Effect 執行、
// D26 分段、投影（B1）、遮蔽、慢訂閱者（B3）、關閉與恢復見
// docs/architecture/hub.md。
package hub
