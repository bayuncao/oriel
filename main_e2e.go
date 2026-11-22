//go:build e2e

package main

// 在應用裡驗收（docs/architecture/testing.md）：帶 e2e 標記的構建引入 MyGo 的 automation 插件，設了
// MYGO_AUTOMATION=<socket> 時另一個行程（mygo-automation）可以列窗口、在頁面裡執行腳本、截圖。
// 發布構建不帶這個標記；沒有 inspector 的構建（mygo build 不加 -debug）即使帶了也不監聽。
import _ "github.com/egoist/mygo/plugins/automation"
