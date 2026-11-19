package app

// RemoteAvailable 回答這個構建有沒有遠程：正式版先沒有（D149，DV17）。正式版在編譯時就定了：
// 構建標記 orielhub_production（channel_production.go）只在 production 渠道加上，mygo.config.ts
// 在 production 渠道少了它、或別的渠道多了它時拒絕構建。不看執行時的狀態（如能否寫安裝位置）。
func RemoteAvailable() bool { return !productionBuild }
