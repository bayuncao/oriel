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
// 群組領頭結束後仍送一次 SIGKILL，不留下忽略 SIGTERM 的子孫。
func terminate(cmd *exec.Cmd, done <-chan error, grace time.Duration) {
	signalGroup(cmd, syscall.SIGTERM)
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
		killGroup(cmd)
	case <-t.C:
		killGroup(cmd)
		<-done
	}
}

// killGroup 也用在領頭已被回收之後：群組只要還有成員，ID 就不會
// 被重用；沒有成員時訊號只會得到 ESRCH（除非這個 ID 恰好在其間
// 被新的群組取得，機率可忽略）。
func killGroup(cmd *exec.Cmd) { signalGroup(cmd, syscall.SIGKILL) }

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
