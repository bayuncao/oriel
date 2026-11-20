//go:build unix

package proc

import (
	"io/fs"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// shellSnapshot 表示是否可以向登入 shell 詢問
// 環境。
const shellSnapshot = true

// isolate 在新的 session 中啟動 cmd：獨立的行程群組且無
// 控制終端，因此互動式 shell 無法接管終端並使
// OrielHub 停止。群組 ID 就是 cmd 的 pid。
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminate 結束 cmd 的整個群組：先送 SIGTERM 讓程式自行收尾，
// done（cmd.Wait 的結果）在 grace 內沒有回報就送 SIGKILL。
// 群組領頭結束後以 reapGroup 收掉留下的子孫，包括忽略 SIGTERM 的。
func terminate(cmd *exec.Cmd, done <-chan error, grace time.Duration) {
	signalGroup(cmd, syscall.SIGTERM)
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		signalGroup(cmd, syscall.SIGKILL)
		<-done
	}
	reapGroup(cmd)
}

// reapGroup 在領頭已被回收之後結束群組裡留下的子孫：送 SIGKILL，直到
// 群組沒有成員（ESRCH）。一次 SIGKILL 會漏掉當時正在 fork 出來的行程
// （macOS 上它這時還不在群組裡），它在父行程從 fork 返回之後才在群組
// 裡，所以隔 reapInterval 再送；群組空了就沒有誰能再 fork 出新成員。
// 群組只要還有成員，ID 就不會被重用。最多送 reapTries 次：殺不掉的
// 行程（卡在核心裡）不讓呼叫方一直等。
func reapGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	for range reapTries {
		if syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) != nil {
			return
		}
		time.Sleep(reapInterval)
	}
}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, sig)
	}
}

// signaled 回報終止程式的訊號編號。
func signaled(ps *os.ProcessState) (int, bool) {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return int(ws.Signal()), true
}

func executableMode(m fs.FileMode) bool { return m.Perm()&0o111 != 0 }
