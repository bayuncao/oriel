//go:build unix

package usage

import (
	"os"
	"strings"
)

// zoneName 從 /etc/localtime 指向的檔案取出 IANA 時區名，如 Asia/Shanghai；
// macOS 與多數 Linux 發行版都把它做成指向 zoneinfo 目錄的符號連結。
func zoneName() string {
	target, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
		return name
	}
	return ""
}
