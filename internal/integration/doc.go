// Package integration 接外部服務（D108）：目錄由後端給出，每個集成的狀態是
// 不可用（帶原因碼與參數）、可連接或已連接；前端不自帶服務列表。
//
// 第一個集成是 GitHub，本地優先：檢測本機的 gh 是否裝了、登入了 github.com、
// 有 repo scope（`gh auth status --json`；gh 會向 GitHub 確認 token 還有效，那是
// gh 自己的請求，OrielHub 不發任何請求）；“連接”只是檢測通過後記下啟用，
// 不存 token、不走 OAuth、不開回調埠。啟用與否經 Store 存在 store 的
// 設定表；狀態變化經 Watch 推給頁面（按 Rev 合併）。
//
// 依賴方向：transport → integration → proc、task（docs/architecture/README.md）。
// 驗收場景見 docs/acceptance/settings.md 第 1 節（IG）。
package integration
