package app

import (
	"testing"

	"github.com/bayuncao/oriel/internal/updater"
)

// U01：測試二進位是開發構建，也沒編進更新源，經真的 mygo.Updater 的
// updater 是 Unavailable：不排後台檢查，Check 不檢查。
func TestNewUpdaterUnavailable(t *testing.T) {
	u := NewUpdater()
	t.Cleanup(u.Close)
	u.Start()
	if s := u.Status(); s.Phase != updater.Unavailable {
		t.Fatalf("status %+v", s)
	}
	if s, err := u.Check(ctxb2()); err != nil || s.Phase != updater.Unavailable {
		t.Fatalf("Check: %+v %v", s, err)
	}
}
