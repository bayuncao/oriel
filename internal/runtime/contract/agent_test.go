package contract

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if Main() {
		return
	}
	os.Exit(m.Run())
}

// K04：錄製代理把自己的命令列參數原樣交給真正的 agent，不論轉接器把它的參數（pi 的 --session-id、
// -e 擴充，claude 的 --resume）加在 Launch 參數的前面還是後面；含空白的參數不拆開。真正的 agent
// 是一段 sh，把收到的參數逐行寫進檔案。
func TestRecordForwardsArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"轉接器的參數在後", []string{"--mode", "rpc", "--session-id", "s-1", "-e", "/tmp/ext one.ts"}},
		{"轉接器的參數在前", []string{"app-server", "--listen", "stdio://", "a b"}},
		{"沒有參數", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			got := filepath.Join(dir, "args")
			agent := filepath.Join(dir, "agent")
			script := "#!/bin/sh\n: > \"$ARGS_OUT\"\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$ARGS_OUT\"; done\n"
			if err := os.WriteFile(agent, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], tc.args...)
			cmd.Env = append(os.Environ(), "ARGS_OUT="+got,
				EnvRecordTo+"="+filepath.Join(dir, "raw.ndjson"), EnvRecordExe+"="+agent)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("proxy: %v\n%s", err, out)
			}
			b, err := os.ReadFile(got)
			if err != nil {
				t.Fatal(err)
			}
			var lines []string
			if s := strings.TrimSuffix(string(b), "\n"); s != "" {
				lines = strings.Split(s, "\n")
			}
			if !slices.Equal(lines, tc.args) {
				t.Fatalf("agent got %q, want %q", lines, tc.args)
			}
		})
	}
}
