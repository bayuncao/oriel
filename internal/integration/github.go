package integration

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/bayuncao/oriel/internal/proc"
)

// githubScope 是 GitHub 集成要的 token scope：讀 Task 開出的 PR 上的 CI 結果與評審意見（D82）。
const githubScope = "repo"

// ghStatusArgs 只問 github.com 上作用中的帳號；加 --json 時不論登入與否都以 0 結束，
// 不帶 --show-token（IG14）。gh 會向 GitHub 確認 token 還有效（這是 gh 自己的請求，
// OrielHub 不發任何請求）。gh 2.105.0 上核實過輸出的形狀（ghStatus）。
var ghStatusArgs = []string{"auth", "status", "--json", "hosts", "--hostname", "github.com", "--active"}

// ghEnv 讓 gh 不互動、不檢查更新、不上色。
var ghEnv = []string{"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1"}

// ghStatus 是 `gh auth status --json hosts` 的輸出。
type ghStatus struct {
	Hosts map[string][]ghHost `json:"hosts"`
}

// ghHost 是一台主機上的一個帳號。
type ghHost struct {
	// State 是 "success"（登入正常）、"timeout"（向 GitHub 確認時逾時）或 "error"
	// （確認失敗：連不上，或 token 失效；原因在 Error）。
	State  string `json:"state"`
	Error  string `json:"error"`
	Active bool   `json:"active"`
	Login  string `json:"login"`
	Scopes string `json:"scopes"` // 逗號分隔，如 "gist, read:org, repo"；fine-grained token 沒有，是空的
}

// checkGitHub 檢測本機的 gh：裝了、登入了 github.com、有 repo scope。只有 ctx 結束
// 或 Runner 關閉時回傳錯誤（這次檢測不算數）；其他結果都寫在 detection 裡。
func (s *Service) checkGitHub(ctx context.Context) (detection, error) {
	env, err := s.o.Env(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return detection{}, ctx.Err()
		}
		return detection{problem: fail(CodeGhFailed, err, "detail", err.Error())}, nil
	}
	paths := proc.Look("gh", proc.SearchDirs(env, s.o.Root))
	if len(paths) == 0 {
		return detection{problem: fail(CodeGhMissing, nil)}, nil
	}
	res := s.o.Runner.Run(ctx, proc.Probe{Path: paths[0], Args: ghStatusArgs, Env: env.With(ghEnv...), Timeout: s.o.Timeout})
	if res.Err != nil {
		if ctx.Err() != nil {
			return detection{}, ctx.Err()
		}
		if errors.Is(res.Err, proc.ErrClosed) {
			return detection{}, res.Err
		}
		return detection{problem: fail(CodeGhFailed, res.Err, "detail", ghDetail(res))}, nil
	}
	var st ghStatus
	if err := json.Unmarshal(res.Stdout, &st); err != nil {
		return detection{problem: fail(CodeGhFailed, err, "detail", err.Error())}, nil
	}
	hosts := st.Hosts["github.com"]
	i := slices.IndexFunc(hosts, func(h ghHost) bool { return h.Active })
	if i < 0 {
		return detection{problem: fail(CodeGhLoggedOut, nil)}, nil
	}
	h := hosts[i]
	switch h.State {
	case "success":
	case "timeout", "error":
		// gh 沒能向 GitHub 確認登入：多半是網路或代理，也可能 token 失效；原因照 gh 說的寫（IG16）。
		return detection{problem: fail(CodeGhUnreachable, nil, "detail", clip(cmp.Or(h.Error, h.State)))}, nil
	default:
		return detection{problem: fail(CodeGhLoggedOut, nil)}, nil
	}
	// fine-grained token 不報 scope（空的）：讀不出權限，算通過，用到時才知道（IG17）。
	if strings.TrimSpace(h.Scopes) != "" && !hasScope(h.Scopes, githubScope) {
		return detection{account: h.Login, problem: fail(CodeGhScopeMissing, nil, "scope", githubScope)}, nil
	}
	return detection{account: h.Login}, nil
}

func hasScope(scopes, want string) bool {
	for s := range strings.SplitSeq(scopes, ",") {
		if strings.TrimSpace(s) == want {
			return true
		}
	}
	return false
}

// ghDetail 是 gh 沒給出看得懂的回答時的說明：逾時就說逾時（stderr 裡可能只是無關的
// 提示）；否則是 stderr 最後一行不空的，沒有 stderr 時用錯誤本身的說明。
func ghDetail(res proc.Result) string {
	if res.TimedOut {
		return "timed out"
	}
	lines := strings.Split(strings.TrimSpace(string(res.Stderr)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		last = res.Err.Error()
	}
	return clip(last)
}

// clip 把說明截到至多 200 個字元（不切開多位元組的字元）。
func clip(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}
