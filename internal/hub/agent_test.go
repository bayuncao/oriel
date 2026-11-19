package hub

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain：以 ORIELHUB_TEST_AGENT=1 重啟自己時跑測試 agent。
func TestMain(m *testing.M) {
	if os.Getenv("ORIELHUB_TEST_AGENT") == "1" {
		runAgent()
		return
	}
	os.Exit(m.Run())
}

// runAgent 是測試 agent：一行一個 JSON。先把 bound 印出來，
// 之後依 prompt 的腳本（; 分隔）動作。它不會返回：行程在 stdin 結束
// （readInput）或腳本 crash 時結束。
func runAgent() {
	dir := os.Getenv("ORIELHUB_TEST_DIR")
	if os.Getenv("ORIELHUB_TEST_EARLY_EXIT") != "" { // 綁定前就結束（D102）
		fmt.Fprintln(os.Stderr, "not logged in")
		os.Exit(2)
	}
	// 先開始讀 stdin：等啟動門時 stdin 結束也照樣結束。
	in := readInput()
	if gate := os.Getenv("ORIELHUB_TEST_START_GATE"); gate != "" {
		for !exists(gate) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	resume := os.Getenv("ORIELHUB_TEST_RESUME")
	id := "n-" + strconv.Itoa(os.Getpid())
	if resume != "" {
		id = resume
	}
	// ORIELHUB_TEST_MODELS（逗號分隔）時報告模型設置：每次啟動（含接回）都從第一個開始，像 OpenCode（D118）。
	var models []string
	if v := os.Getenv("ORIELHUB_TEST_MODELS"); v != "" {
		models = strings.Split(v, ",")
		writeLine(map[string]any{"type": "model", "current": models[0], "choices": models})
	}
	writeLine(map[string]any{"type": "bound", "id": id})

	for m := range in {
		if m["type"] == "set_model" {
			// 換模型記進 prompts.log（以 model: 開頭），看得出它與 prompt 的先後；broken 一律拒絕。
			v := text(m, "value")
			if dir != "" {
				f, _ := os.OpenFile(filepath.Join(dir, "prompts.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
				if f != nil {
					fmt.Fprintln(f, "model: "+v)
					f.Close()
				}
			}
			// ORIELHUB_TEST_MODEL_GATE：檔案出現前不回應（換模型的途中）。
			if gate := os.Getenv("ORIELHUB_TEST_MODEL_GATE"); gate != "" {
				for !exists(gate) {
					time.Sleep(10 * time.Millisecond)
				}
			}
			if v == "broken" || !slices.Contains(models, v) {
				writeLine(map[string]any{"type": "model_error", "message": "cannot use " + v})
				continue
			}
			writeLine(map[string]any{"type": "model_set", "current": v, "choices": models})
			continue
		}
		if m["type"] != "prompt" {
			continue
		}
		if dir != "" {
			f, _ := os.OpenFile(filepath.Join(dir, "prompts.log"),
				os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if f != nil {
				fmt.Fprintln(f, m["text"])
				f.Close()
			}
		}
		if !runScript(text(m, "text"), in, dir) {
			os.Exit(0)
		}
	}
}

// readInput 讀 stdin 上一行一則的 JSON 訊息，依序交給回傳的 channel；這個
// channel 永遠不關。
//
// stdin 結束就是連線結束，行程跟著結束：讀完是 0，讀取出錯是 1（原因寫在
// stderr），佇列裡還沒處理的訊息不再處理。stdin 的寫端只有啟動 agent 的一方
// 持有，它只在不再要 agent 的輸出時才關 stdin（lineConn 從不主動關），或者它
// 已經不在了（行程結束時系統會關掉它持有的管道端）。所以不論 agent 在等什麼
// （啟動門、slow 的門、answer、cancel、下一個 prompt）都立刻結束；測試中途被
// 殺時沒有人來殺它（t.Cleanup 不會執行），只能靠這一點。
//
// 讀 stdin 從不等 agent 取走訊息：agent 等門時讀到的訊息先排在佇列裡，不論
// 積了多少，stdin 一結束都看得到。
func readInput() <-chan map[string]any {
	in := make(chan map[string]any)
	read := make(chan map[string]any)
	go func() { // 佇列：隨時收 read，有排隊的就交給 in
		var queue []map[string]any
		for {
			var next chan<- map[string]any
			var head map[string]any
			if len(queue) > 0 {
				next, head = in, queue[0]
			}
			select {
			case m := <-read:
				queue = append(queue, m)
			case next <- head:
				queue[0] = nil // 交出去的不再留著
				queue = queue[1:]
			}
		}
	}()
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				read <- m
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "test agent: reading stdin:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
	return in
}

// runScript 執行一段腳本；回傳 false 表示行程要結束。
func runScript(script string, in <-chan map[string]any, dir string) bool {
	for _, part := range strings.Split(script, ";") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), fields[0]))
		switch fields[0] {
		case "say":
			writeLine(map[string]any{"type": "text", "text": rest})
		case "think":
			writeLine(map[string]any{"type": "thinking", "text": rest})
		case "tool":
			writeLine(map[string]any{"type": "tool", "id": fields[1], "title": rest})
		case "use":
			// use ID KIND PATH...：帶分類與路徑的工具呼叫（用過的檔案，D107）。
			writeLine(map[string]any{"type": "tool", "id": fields[1], "title": fields[2],
				"kind": fields[2], "paths": fields[3:]})
			writeLine(map[string]any{"type": "tool_update", "id": fields[1], "status": "completed"})
		case "fail":
			// fail ID KIND PATH...：同 use，但工具呼叫失敗（例如權限被拒），不算用過（D107）。
			writeLine(map[string]any{"type": "tool", "id": fields[1], "title": fields[2],
				"kind": fields[2], "paths": fields[3:]})
			writeLine(map[string]any{"type": "tool_update", "id": fields[1], "status": "failed"})
		case "ask":
			// ask ID CMD：等對應的 answer，拒絕就 end failed。
			id := fields[1]
			writeLine(map[string]any{"type": "ask", "id": id,
				"command": strings.Join(fields[2:], " ")})
			for m := range in {
				if m["type"] == "answer" && m["id"] == id {
					if m["verdict"] != "allow" {
						writeLine(map[string]any{"type": "end", "result": "failed"})
						return true // 回合結束，行程繼續等下一個 prompt
					}
					break
				}
			}
		case "crash":
			fmt.Fprintln(os.Stderr, "boom")
			os.Exit(3)
		case "hang":
			// 等 cancel；stdin 結束時行程直接結束（readInput）。
			for m := range in {
				if m["type"] == "cancel" {
					writeLine(map[string]any{"type": "end", "result": "cancelled"})
					return true
				}
			}
		case "slow":
			n, _ := strconv.Atoi(fields[1])
			for i := 1; i <= n; i++ {
				writeLine(map[string]any{"type": "text", "text": "c" + strconv.Itoa(i)})
				for !exists(filepath.Join(dir, "go-"+strconv.Itoa(i))) {
					time.Sleep(10 * time.Millisecond)
				}
			}
		}
	}
	writeLine(map[string]any{"type": "end", "result": "completed"})
	return true
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeLine(m map[string]any) {
	b, _ := json.Marshal(m)
	fmt.Println(string(b))
}

func text(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// F：測試 agent 的壽命跟著 stdin。啟動它的一方關掉 stdin，或那一方已經不在
// （行程結束時系統會關掉它持有的管道端），agent 就結束，不論它正在等什麼。
// 測試中途被殺時 t.Cleanup 不會執行、沒有人來殺 agent，只能靠這一點。
func TestAgentExitsWhenStdinCloses(t *testing.T) {
	typeIs := func(typ string) func(map[string]any) bool {
		return func(m map[string]any) bool { return m["type"] == typ }
	}
	textIs := func(s string) func(map[string]any) bool {
		return func(m map[string]any) bool { return m["type"] == "text" && m["text"] == s }
	}
	for _, tc := range []struct {
		name      string
		startGate bool                      // 設啟動門，門始終不開
		prompt    string                    // 先送的 prompt；"" 不送
		until     func(map[string]any) bool // 等 agent 印出這一行才往下；nil 不等
		backlog   int                       // 之後再送幾則 agent 不會取走的訊息
		raw       string                    // 之後再送的原始輸入
		status    int                       // 期望的結束狀態
		stderr    string                    // stderr 要包含的文字
	}{
		{name: "F1 等啟動門", startGate: true},
		{name: "F2 slow 印出 c1 後等第一道門", prompt: "slow 1", until: textIs("c1")},
		{name: "F3 兩個 prompt 之間", prompt: "say x", until: typeIs("end")},
		{name: "F4 回合中 hang", prompt: "say x; hang", until: textIs("x")},
		{name: "F5 等 answer", prompt: "ask a1 ls", until: typeIs("ask")},
		{name: "F6 等門時積了 100 則沒取走的訊息", prompt: "slow 1", until: textIs("c1"), backlog: 100},
		{name: "F7 一行 1 MiB 以上", until: typeIs("bound"), raw: strings.Repeat("x", 2<<20) + "\n",
			status: 1, stderr: "reading stdin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			env := []string{"ORIELHUB_TEST_DIR=" + dir}
			if tc.startGate {
				env = append(env, "ORIELHUB_TEST_START_GATE="+filepath.Join(dir, "start-gate"))
			}
			a := startAgent(t, env...)
			if tc.prompt != "" {
				a.send(t, map[string]any{"type": "prompt", "text": tc.prompt})
			}
			if tc.until != nil {
				a.waitLine(t, tc.until)
			}
			for range tc.backlog {
				a.send(t, map[string]any{"type": "noop"}) // 讀到也不理的種類
			}
			if tc.raw != "" {
				// agent 讀到一半出錯就結束，剩下的寫不進去（EPIPE）：不理，看結束狀態。
				_, _ = a.stdin.WriteString(tc.raw)
			}
			a.stdin.Close()
			status, stderr := a.exit(t)
			if status != tc.status || !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("exit status %d, stderr %q; want %d with %q", status, stderr, tc.status, tc.stderr)
			}
		})
	}
}

// F8：agent 等門時積下的訊息不丟也不亂：門開之後依序全部處理，之後關 stdin
// 照常以 0 結束。
func TestAgentRunsQueuedPromptsInOrder(t *testing.T) {
	const n = 200 // 比舊的 64 格 channel 多
	dir := t.TempDir()
	a := startAgent(t, "ORIELHUB_TEST_DIR="+dir)
	a.send(t, map[string]any{"type": "prompt", "text": "slow 1"})
	a.waitLine(t, func(m map[string]any) bool { return m["type"] == "text" && m["text"] == "c1" })
	for i := range n {
		a.send(t, map[string]any{"type": "prompt", "text": "say p" + strconv.Itoa(i)})
	}
	if err := os.WriteFile(filepath.Join(dir, "go-1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	a.waitLine(t, func(m map[string]any) bool {
		if m["type"] == "text" {
			got = append(got, text(m, "text"))
		}
		return len(got) == n
	})
	for i, s := range got {
		if want := "p" + strconv.Itoa(i); s != want {
			t.Fatalf("text %d is %q, want %q (all: %q)", i, s, want, got)
		}
	}
	a.stdin.Close()
	if status, stderr := a.exit(t); status != 0 {
		t.Fatalf("exit status %d, stderr %q; want 0", status, stderr)
	}
}

// agentProc 是直接啟動的測試 agent（不經 lineRuntime）：測試持有它 stdin 的
// 寫端與 stdout 的讀端。
type agentProc struct {
	cmd    *exec.Cmd
	stdin  *os.File
	out    *os.File // stdout 的讀端
	lines  *bufio.Scanner
	stderr bytes.Buffer
	done   chan struct{} // 行程結束時關閉
	err    error         // Wait 的結果，done 關閉前寫入
}

// startAgent 啟動測試 agent，env 加在繼承的環境之後。測試結束時殺掉它並等它
// 結束，場景失敗也不留下行程。
func startAgent(t *testing.T, env ...string) *agentProc {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		t.Fatal(err)
	}
	// 寫入都有期限：agent 不讀時寫滿管道，也不會把測試卡到 go test 的逾時。
	if err := inW.SetWriteDeadline(time.Now().Add(testWait)); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		t.Fatal(err)
	}
	a := &agentProc{cmd: exec.Command(os.Args[0]), stdin: inW, out: outR, done: make(chan struct{})}
	// -race 下以 0 結束的行程會先等 atexit_sleep_ms（預設 1 秒）才離開，其間
	// 別的 goroutine 照常在跑：設成 0，讀完 stdin 就立刻結束。保留繼承的選項。
	gorace := strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0")
	a.cmd.Env = append(append(os.Environ(), "ORIELHUB_TEST_AGENT=1", "GORACE="+gorace), env...)
	a.cmd.Stdin, a.cmd.Stdout, a.cmd.Stderr = inR, outW, &a.stderr
	err = a.cmd.Start()
	// 子行程有了自己的一份；這邊只留 stdin 的寫端（唯一的一個，關了 agent
	// 才讀得到 EOF）與 stdout 的讀端。
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		t.Fatal(err)
	}
	a.lines = bufio.NewScanner(outR)
	go func() {
		a.err = a.cmd.Wait()
		close(a.done)
	}()
	t.Cleanup(func() {
		_ = a.cmd.Process.Kill() // 已經結束時回錯，不理
		<-a.done
		inW.Close()
		outR.Close()
	})
	return a
}

// send 寫一則訊息到 agent 的 stdin。
func (a *agentProc) send(t *testing.T, m map[string]any) {
	t.Helper()
	b, _ := json.Marshal(m)
	if _, err := a.stdin.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// waitLine 讀 agent 的輸出到 match 的那一行；testWait 內沒讀到就失敗。
func (a *agentProc) waitLine(t *testing.T, match func(map[string]any) bool) {
	t.Helper()
	if err := a.out.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatal(err)
	}
	for a.lines.Scan() {
		var m map[string]any
		if json.Unmarshal(a.lines.Bytes(), &m) == nil && match(m) {
			return
		}
	}
	t.Fatalf("agent output ended before the expected line: %v", a.lines.Err())
}

// exit 等 agent 結束，回傳結束狀態（被訊號結束時是 -1）與 stderr；testWait 內
// 沒結束就失敗（t.Cleanup 會殺掉它）。
func (a *agentProc) exit(t *testing.T) (int, string) {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(testWait):
		t.Fatalf("test agent still running %v after its stdin closed", testWait)
	}
	var ee *exec.ExitError
	switch {
	case a.err == nil:
		return 0, a.stderr.String()
	case errors.As(a.err, &ee):
		return ee.ExitCode(), a.stderr.String()
	}
	t.Fatalf("waiting for the test agent: %v", a.err)
	return 0, ""
}
