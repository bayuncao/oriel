// Package pi 是內建的 pi coding agent：agent.Definition、本機用量記錄，以及經 `pi --mode rpc`
// （stdio 上的 JSONL）驅動 pi 的 hub.Runtime。pi 自己不做審批，審批經 OrielHub 時由內嵌的擴充
// （approvals.ts）在工具執行前詢問。協議見 docs/research/pi-rpc.md，場景見 docs/acceptance/m3.md 第 3 節。
package pi
