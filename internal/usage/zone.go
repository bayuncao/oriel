package usage

import (
	"os"
	"strings"
	"time"
)

// SystemZone 是現在的系統時區。Go 的 time.Local 在行程啟動時就定下了，之後改
// 系統時區也不跟著變（U09），所以每次都重新取：設了 TZ 時以它為準，否則讀系統
// 的時區設定（zoneName，依平台）；都取不到時退回 time.Local。
func SystemZone() *time.Location {
	if tz, ok := os.LookupEnv("TZ"); ok {
		if tz == "" {
			return time.UTC
		}
		if loc, err := time.LoadLocation(strings.TrimPrefix(tz, ":")); err == nil {
			return loc
		}
	}
	if name := zoneName(); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return time.Local
}
