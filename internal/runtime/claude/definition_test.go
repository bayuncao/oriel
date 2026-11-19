package claude

import (
	"slices"
	"testing"
)

// TestLaunchPinsPermissionMode 確認兩種啟動都走 stdio 審批並明確
// 指定權限模式，不受設定與版本的起始模式影響。
func TestLaunchPinsPermissionMode(t *testing.T) {
	l := Definition().Launch
	for _, c := range []struct {
		args []string
		mode string
	}{{l.Args, "default"}, {l.Native, "bypassPermissions"}} {
		if i := slices.Index(c.args, "--permission-prompt-tool"); i < 0 || c.args[i+1] != "stdio" {
			t.Errorf("%q: no --permission-prompt-tool stdio", c.args)
		}
		if i := slices.Index(c.args, "--permission-mode"); i < 0 || c.args[i+1] != c.mode || slices.Index(c.args[i+1:], "--permission-mode") >= 0 {
			t.Errorf("%q: want exactly one --permission-mode %s", c.args, c.mode)
		}
	}
}
