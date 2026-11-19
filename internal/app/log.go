package app

import (
	"os"
	"path/filepath"
)

const logMax = 10 << 20 // 10 MiB

// OpenLog 開啟日誌檔：超過 10 MiB 先輪轉成 .1（覆蓋舊的）。
func OpenLog(dir string) (*os.File, error) {
	path := filepath.Join(dir, "orielhub.log")
	if st, err := os.Stat(path); err == nil && st.Size() > logMax {
		os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
