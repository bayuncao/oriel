//go:build !unix

package proc

import (
	"io/fs"
	"os"
	"os/exec"
	"time"
)

// shellSnapshot 為 false：沒有登入 shell 可詢問，且 app 會繼承
// 使用者的環境。
const shellSnapshot = false

func isolate(*exec.Cmd) {}

// terminate 直接結束程式：這些平台沒有 SIGTERM；行程群組
// （Windows 的 job object）尚未實作。
func terminate(cmd *exec.Cmd, done <-chan error, _ time.Duration) {
	reapGroup(cmd)
	<-done
}

func reapGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func signaled(*os.ProcessState) (int, bool) { return 0, false }

func executableMode(fs.FileMode) bool { return true }
