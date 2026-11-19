//go:build darwin

package workspace

import (
	"context"
	"path/filepath"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
)

// usable 確認找到的 git 真的能跑：/usr/bin/git 是 CLT 的
// stub，未安裝時啟動它會彈出系統安裝對話框（D45）。
// 成功記進快取；失敗不記（使用者可能之後才裝）。
func (g *Git) usable(ctx context.Context, env proc.Env, gitPath string) bool {
	if filepath.Dir(gitPath) != filepath.Join(g.root, "/usr/bin") {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cltOK {
		return true
	}
	res := g.runner.Run(ctx, proc.Probe{
		Path:    filepath.Join(g.root, "/usr/bin/xcode-select"),
		Args:    []string{"-p"},
		Env:     env,
		Timeout: 10 * time.Second,
	})
	if res.ExitCode != 0 {
		return false
	}
	g.cltOK = true
	return true
}
