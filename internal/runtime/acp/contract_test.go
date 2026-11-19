//go:build unix

package acp

import (
	"context"
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

// TestMain：以 contract 的環境變數重啟自己時扮演 agent 或錄製代理。
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

// K01–K07：契約用例。預設回放 testdata/contract；ORIELHUB_LIVE_AGENTS 對真實 CLI 跑。
func TestContract(t *testing.T) {
	r := newRuntime(t, Options{})
	contract.Run(t, contract.Suite{
		Runtime: r,
		Dir:     "testdata/contract",
		Live:    func(t *testing.T, id string) (task.LaunchSnapshot, bool) { return liveLaunch(t, r, id) },
		Check:   checkOptions,
	})
}

// liveLaunch 找本機裝的內建 agent，探測它的能力（D98），做成 Launch。
func liveLaunch(t *testing.T, r *Runtime, id string) (task.LaunchSnapshot, bool) {
	t.Helper()
	var def agent.Definition
	for _, d := range []agent.Definition{Grok(), Gemini(), OpenCode(), Cursor(), Copilot(), Qwen()} {
		if string(d.ID) == id {
			def = d
		}
	}
	if def.ID == "" {
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
	caps, err := r.Capabilities(context.Background(), proc.Command{Path: path, Args: def.Launch.Args,
		Env: env, Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("probing %s: %v", id, err)
	}
	t.Logf("%s at %s, caps %+v", id, path, caps)
	return task.LaunchSnapshot{Agent: def.ID, AgentName: def.Name, Protocol: task.ProtocolACP,
		Executable: path, Args: def.Launch.Args, Approval: task.ApprovalOrielHub, Caps: caps}, true
}

// checkOptions：真實 agent 的每個權限請求都要有 allow_once（K07、D99），
// 否則允許只能落到 allow_always，讓 agent 自己記下放行。
func checkOptions(t *testing.T, c string, traffic []contract.Line) {
	t.Helper()
	for _, l := range traffic {
		if l.Agent == nil {
			continue
		}
		var m struct {
			Method string `json:"method"`
			Params struct {
				Options []struct {
					Kind string `json:"kind"`
				} `json:"options"`
			} `json:"params"`
		}
		if json.Unmarshal(l.Agent, &m) != nil || m.Method != "session/request_permission" {
			continue
		}
		found := false
		for _, o := range m.Params.Options {
			found = found || o.Kind == "allow_once"
		}
		if !found {
			t.Errorf("%s: a permission request without allow_once: %s", c, l.Agent)
		}
	}
}
