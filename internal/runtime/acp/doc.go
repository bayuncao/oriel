// Package acp 匯出使用 Agent Client Protocol 的內建 agent
// （Grok、Gemini、OpenCode、Cursor、Copilot、Qwen）的
// agent.Definition 與本機用量記錄的讀取，各一個檔案；以及通用的
// ACP 轉接器（Runtime，實現 hub.Runtime）：每個 Session 一個 agent
// 行程，stdio 上的 JSON-RPC 由 github.com/coder/acp-go-sdk 處理（D96）。
// 規則見 docs/architecture/runtime.md，場景見 docs/acceptance/m2.md。
package acp
