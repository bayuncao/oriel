package contract

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// 測試二進位以這些環境變數重啟自己時扮演 agent（K02）或錄製代理（K04）。
const (
	// EnvReplay 是要回放的夾具路徑；逗號分隔的幾個時，依次給每一次啟動
	// （同一個 Session 恢復後換下一個），用到第幾個記在 EnvReplayState 的檔案裡。
	EnvReplay      = "ORIELHUB_CONTRACT_REPLAY"
	EnvReplayState = "ORIELHUB_CONTRACT_REPLAY_STATE"
	EnvRecordTo    = "ORIELHUB_CONTRACT_RECORD" // 錄下來的原始訊息寫到這裡
	EnvRecordExe   = "ORIELHUB_CONTRACT_EXE"    // 真正的 agent 可執行檔；參數是代理自己的命令列
)

// Main 在測試二進位被重啟為 agent 或錄製代理時執行它並結束行程；否則回
// false，由呼叫者照常跑測試。在 TestMain 的最前面呼叫。
func Main() bool {
	// 錄製代理先於回放：代理去掉自己的變數再啟動 agent，所以也能錄回放的 agent。
	if out := os.Getenv(EnvRecordTo); out != "" {
		os.Exit(record(out, os.Getenv(EnvRecordExe)))
	}
	if paths := os.Getenv(EnvReplay); paths != "" {
		path, err := pick(paths, os.Getenv(EnvReplayState))
		if err != nil {
			fmt.Fprintln(os.Stderr, "contract:", err)
			os.Exit(98)
		}
		os.Exit(replay(path))
	}
	return false
}

// pick 從逗號分隔的夾具中取這一次的：第 n 次啟動用第 n 個，n 記在 state 檔裡。
func pick(paths, state string) (string, error) {
	list := strings.Split(paths, ",")
	if len(list) == 1 {
		return list[0], nil
	}
	if state == "" {
		return "", fmt.Errorf("%s needs %s", EnvReplay, EnvReplayState)
	}
	n := 0
	if b, err := os.ReadFile(state); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if n >= len(list) {
		return "", fmt.Errorf("started %d times, only %d fixtures", n+1, len(list))
	}
	if err := os.WriteFile(state, []byte(strconv.Itoa(n+1)), 0o600); err != nil {
		return "", err
	}
	return list[n], nil
}

// replay 依夾具扮演 agent：agent 行寫到 stdout；client 行讀 stdin 的下一則
// 訊息並核對；stderr 行寫到 stderr；exit 行結束。核對不符時把原因寫到
// stderr、以 97 結束，測試從 exited 的 stderr 尾部看得到。夾具走完後讀到
// stdin 結束為止，再以 0 結束。
func replay(path string) int {
	lines, err := ReadFixture(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "contract:", err)
		return 98
	}
	cwd, _ := os.Getwd()
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 16<<20)
	ids := map[string]any{} // 夾具裡 OrielHub 的請求 ID → 這次的 ID
	out := bufio.NewWriter(os.Stdout)
	for i, l := range lines {
		switch {
		case l.Agent != nil:
			v, err := substitute(l.Agent, cwd)
			if err != nil {
				return mismatch(i, "bad agent line: %v", err)
			}
			if m, ok := v.(map[string]any); ok && m["method"] == nil {
				if id, ok := ids[fmt.Sprint(m["id"])]; ok {
					m["id"] = id // 回應用這次的請求 ID
				}
			}
			b, _ := json.Marshal(v)
			out.Write(append(b, '\n'))
			out.Flush()
		case l.Client != nil:
			want, err := substitute(l.Client, cwd)
			if err != nil {
				return mismatch(i, "bad client line: %v", err)
			}
			got, ok := next(in)
			if !ok {
				return mismatch(i, "stdin ended, want %s", l.Client)
			}
			w, _ := want.(map[string]any)
			wantID, hasID := w["id"]
			delete(w, "id")
			if hasID && w["method"] != nil {
				ids[fmt.Sprint(wantID)] = got["id"] // 請求：記下這次的 ID
			} else if hasID && fmt.Sprint(got["id"]) != fmt.Sprint(wantID) {
				return mismatch(i, "response id %v, want %v", got["id"], wantID)
			}
			if !matches(w, got) {
				g, _ := json.Marshal(got)
				return mismatch(i, "got %s, want %s", g, l.Client)
			}
		case l.Stderr != "":
			fmt.Fprintln(os.Stderr, l.Stderr)
		case l.Raw != "":
			out.WriteString(l.Raw + "\n")
			out.Flush()
		case l.Touch != "":
			out.Flush()
			if err := os.WriteFile(l.Touch, nil, 0o600); err != nil {
				return mismatch(i, "touch: %v", err)
			}
		case l.CloseStdout:
			out.Flush()
			os.Stdout.Close()
		case l.CloseStdin:
			os.Stdin.Close()
		case l.Hold:
			out.Flush()
			// 等轉接器結束行程群組（SIGTERM）；只是 select{} 的話 runtime 會判成死鎖而結束。
			stop := make(chan os.Signal, 1)
			signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
			<-stop
			return 143
		case l.EchoEnv != "":
			fmt.Fprintf(os.Stderr, "%s=%s\n", l.EchoEnv, os.Getenv(l.EchoEnv))
		case l.Exit != nil:
			out.Flush()
			return *l.Exit
		}
	}
	for in.Scan() {
	}
	return 0
}

// next 讀 stdin 的下一則訊息；取消請求（SDK 在請求的 ctx 結束時送）不算。
func next(in *bufio.Scanner) (map[string]any, bool) {
	for in.Scan() {
		var m map[string]any
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		if m["method"] == "$/cancel_request" {
			continue
		}
		return m, true
	}
	return nil, false
}

func mismatch(i int, format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "contract: fixture line %d: %s\n", i+1, fmt.Sprintf(format, args...))
	return 97
}

// record 以代理自己的命令列參數原樣啟動真正的 agent exe（轉接器組出的參數
// 都在這裡，不論它加在 Launch 參數的前後），原樣轉送 stdio，並把兩個方向的
// 每一則訊息依序寫到 out（{"client": …} 或 {"agent": …}）。代理與 agent 在
// 同一個行程群組，轉接器結束群組時一起結束；每行立刻寫出，不靠結束時清空緩衝。
func record(out, exe string) int {
	if exe == "" {
		fmt.Fprintln(os.Stderr, "contract:", EnvRecordExe, "is empty")
		return 98
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "contract:", err)
		return 98
	}
	defer f.Close()
	var mu sync.Mutex
	log := func(dir string, line []byte) {
		if !json.Valid(line) {
			return
		}
		b, _ := json.Marshal(map[string]json.RawMessage{dir: line})
		mu.Lock()
		f.Write(append(b, '\n'))
		mu.Unlock()
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, EnvRecordTo+"=") && !strings.HasPrefix(kv, EnvRecordExe+"=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 98
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 98
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "contract:", err)
		return 98
	}
	go func() {
		copyLines(os.Stdin, stdin, func(l []byte) { log("client", l) })
		stdin.Close()
	}()
	copyLines(stdout, os.Stdout, func(l []byte) { log("agent", l) })
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}

func copyLines(r io.Reader, w io.Writer, each func([]byte)) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			each(append([]byte(nil), line[:len(line)-trailingNewline(line)]...))
			if _, werr := w.Write(line); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func trailingNewline(b []byte) int {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return 1
	}
	return 0
}
