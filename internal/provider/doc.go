// Package provider 讀各 AI 廠商公開的狀態頁，算出當前狀態、最近 30 天與正在進行的
// 事故，給首頁功能區的 Provider status（docs/architecture/provider.md、m2.md 第 23 節、
// D131）。
//
// 只向公開的狀態頁發 GET，不調用廠商的 API、不讀任何鑰匙、不帶 cookie。不導入
// mygo：HTTP client（帶本機代理）、時鐘、時區與電腦醒來的通知由 app 注入。各家
// 狀態頁的格式是公開的（Statuspage、Flashduty 的 RSS、Google Cloud、AWS Health），
// 解析都在本包裡。
package provider
