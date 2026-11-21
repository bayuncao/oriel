//go:build unix

package codex

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/runtime/contract"
	"github.com/bayuncao/oriel/internal/task"
)

// TestMain：以 contract 的環境變數重啟自己時扮演 Codex 或錄製代理。
func TestMain(m *testing.M) {
	if contract.Main() {
		return
	}
	os.Exit(m.Run())
}

// newRuntime 是測試用的轉接器；Runner 在測試結束時關閉。
func newRuntime(t *testing.T, o Options) *Runtime {
	t.Helper()
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	o.Runner = runner
	if o.Version == "" {
		o.Version = "test"
	}
	return New(o)
}

// K01–K07、CX51、CX52：契約用例。預設回放 testdata/contract；ORIELHUB_LIVE_AGENTS=codex 對本機的
// Codex 跑（會花它的額度）。
func TestContract(t *testing.T) {
	r := newRuntime(t, Options{})
	contract.Run(t, contract.Suite{
		Runtime: r,
		Dir:     "testdata/contract",
		Live:    liveLaunch,
		Check:   checkAnswers,
	})
}

// liveLaunch 找本機裝的 Codex，照內建的 Definition 做成 Launch。
func liveLaunch(t *testing.T, id string) (task.LaunchSnapshot, bool) {
	t.Helper()
	def := Definition()
	if string(def.ID) != id {
		t.Fatalf("unknown agent %q", id)
	}
	env := proc.Inherited()
	path, err := exec.LookPath(def.Detect.Command)
	if err != nil {
		for _, d := range def.Detect.Dirs {
			if p := filepath.Join(env.Expand(d), def.Detect.Command); proc.Executable(p) {
				path, err = p, nil
				break
			}
		}
	}
	if err != nil {
		return task.LaunchSnapshot{}, false
	}
	return agent.Launch(def, agent.Installation{Path: path}, agent.DefaultProfile(def), def.Caps), true
}

// checkAnswers：真實執行時，OrielHub 對每個審批請求的回答都是 accept 或 decline（CX32）。
func checkAnswers(t *testing.T, c string, traffic []contract.Line) {
	t.Helper()
	for _, l := range traffic {
		if l.Client == nil {
			continue
		}
		var m struct {
			Method string `json:"method"`
			Result *struct {
				Decision json.RawMessage `json:"decision"`
			} `json:"result"`
		}
		if json.Unmarshal(l.Client, &m) != nil || m.Method != "" || m.Result == nil || m.Result.Decision == nil {
			continue
		}
		if d := string(m.Result.Decision); d != `"accept"` && d != `"decline"` {
			t.Errorf("%s: a decision that is neither accept nor decline: %s", c, l.Client)
		}
	}
}
