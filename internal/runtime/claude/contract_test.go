//go:build unix

package claude

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/runtime/contract"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/usage"
)

// TestMain：以 contract 的環境變數重啟自己時扮演 Claude Code 或錄製代理。
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
	return New(o)
}

// K01–K07、CL51、CL52、CQ13：契約用例。預設回放 testdata/contract；ORIELHUB_LIVE_AGENTS=claude 對本機的
// Claude Code 跑（會花它的額度）。回放時 prompt 的夾具帶一則 rate_limit_event：額度照 CQ01 記下，
// 事實流照舊由契約核對。真實執行時用 API key 的 Claude Code 不報額度，不核對。
func TestContract(t *testing.T) {
	var mu sync.Mutex
	var quotas []usage.Quota
	r := newRuntime(t, Options{Quota: func(q usage.Quota) {
		mu.Lock()
		defer mu.Unlock()
		quotas = append(quotas, q)
	}})
	contract.Run(t, contract.Suite{
		Runtime: r,
		Dir:     "testdata/contract",
		Live:    liveLaunch,
		Check:   checkAnswers,
	})
	if os.Getenv(contract.EnvLive) != "" {
		return
	}
	want := []usage.Limit{{Minutes: 300, UsedPercent: 25, ResetsAt: time.Unix(1795000000, 0).UTC()},
		{Minutes: 10080, UsedPercent: 50, ResetsAt: time.Unix(1795500000, 0).UTC()}}
	mu.Lock()
	defer mu.Unlock()
	if len(quotas) == 0 || !slices.Equal(quotas[0].Windows, want) {
		t.Fatalf("quotas %+v, want windows %+v", quotas, want)
	}
}

// liveLaunch 找本機裝的 Claude Code，照內建的 Definition 做成 Launch。
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

// checkAnswers：真實執行時，OrielHub 對每個 can_use_tool 的回答都要是 allow 或 deny（CL31、CL32），
// 允許時帶回 updatedInput。
func checkAnswers(t *testing.T, c string, traffic []contract.Line) {
	t.Helper()
	for _, l := range traffic {
		if l.Client == nil {
			continue
		}
		var m struct {
			Type     string `json:"type"`
			Response struct {
				Response *struct {
					Behavior     string          `json:"behavior"`
					UpdatedInput json.RawMessage `json:"updatedInput"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(l.Client, &m) != nil || m.Type != "control_response" || m.Response.Response == nil {
			continue
		}
		switch a := m.Response.Response; a.Behavior {
		case "allow":
			if len(a.UpdatedInput) == 0 {
				t.Errorf("%s: an allow without updatedInput: %s", c, l.Client)
			}
		case "deny":
		default:
			t.Errorf("%s: an answer that is neither allow nor deny: %s", c, l.Client)
		}
	}
}
