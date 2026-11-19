// Package task 是 OrielHub 的純領域核心。
//
// Task 是唯一的聚合：透過 OrielHub 分派的工作項目
// （例如「實作登入頁面」）。Session 是 Task 內部的實體，
// 每個 Session 各與一個 agent 對話，因此所有橫跨 task 與
// 其 session 的規則（狀態、fan-out、worktree 完成）都以原子方式決定。
//
// Task 由其只增不減的 Event Stream 與三個純函式定義：
//
//	Decide(State, Inputs, Command) ([]Event, error) // 業務規則
//	Evolve(State, Event) State                      // 摺疊一個事件
//	Plan(State, Live) []Effect                      // 調和期望狀態與執行中狀態
//
// roadmap 的 M0 已完成：Evolve、事件編解碼（EncodeEvent、DecodeEvent 與 upcaster）、
// 完整的 Decide（Task、Session 與回合、審批與 Rule、確認、Effect 失敗、合併、扇出與審查、
// 刪除）與 Plan。
//
// 規則見 docs/architecture/domain.md。此套件不匯入其他內部套件、
// 不執行 I/O、不讀取時鐘，也不啟動 goroutine。
package task
