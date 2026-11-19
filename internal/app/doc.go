// Package app 是 OrielHub 的組合根。它不依賴視窗，
// 因此可以單獨測試，之後也能以無頭方式託管。
//
// New 開啟 store、註冊內建的 agent Definition（agents.go）、啟動
// agent.Discovery，把執行期轉接器、workspace 與 store 接進 hub，並建好
// 用量、Provider 與遠程等部件；Close 依相反順序關閉。測試中它還
// 匯總各套件的錯誤代碼並核對翻譯目錄。
//
// 這裡沒有任何東西可以在 mygo.App.WhenReady 之前執行，因為
// `mygo generate` 會執行 main 直到 App.Run 為止。
package app
