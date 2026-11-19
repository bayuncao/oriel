// Package store 是 hub.Log + hub.Config 的 SQLite 實現：
// 事件流、投影列與設定存在同一個資料庫，Append 是一個
// Transaction（記錄、遮蔽、投影、新 Workspace、新 Rule）。
// 細節見 docs/architecture/store.md。
package store
