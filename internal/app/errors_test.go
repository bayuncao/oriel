package app_test

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/app"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/integration"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/relay"
	"github.com/bayuncao/oriel/internal/runtime/acp"
	"github.com/bayuncao/oriel/internal/runtime/claude"
	"github.com/bayuncao/oriel/internal/runtime/codex"
	"github.com/bayuncao/oriel/internal/runtime/pi"
	"github.com/bayuncao/oriel/internal/store"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
	"github.com/bayuncao/oriel/internal/transport/mygoipc"
	"github.com/bayuncao/oriel/internal/transport/remote"
	"github.com/bayuncao/oriel/internal/updater"
	"github.com/bayuncao/oriel/internal/usage"
	"github.com/bayuncao/oriel/internal/workspace"
)

// errorCode 是一個給使用者看的代碼與它的參數（docs/architecture/README.md 規則 9）。
type errorCode struct {
	code   string
	params []string
}

// errorCodes 匯總各套件的代碼，依代碼排序：app 是唯一認得所有套件的地方。
func errorCodes() []errorCode {
	var out []errorCode
	for _, c := range agent.ProblemCodes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range acp.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range claude.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range codex.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range pi.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range proc.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, r := range task.Reasons() {
		out = append(out, errorCode{r.Code(), nil})
	}
	out = append(out, errorCode{task.CodeMergeConflict, []string{"files"}})
	out = append(out, errorCode{task.CodeDefaultModelUnavailable, nil})
	for _, c := range transport.Codes() {
		out = append(out, errorCode{string(c), nil})
	}
	for _, c := range hub.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range store.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range app.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range mygoipc.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range remote.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range workspace.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range updater.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range usage.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range integration.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range provider.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	for _, c := range relay.Codes() {
		out = append(out, errorCode{string(c), c.Params()})
	}
	slices.SortFunc(out, func(a, b errorCode) int { return strings.Compare(a.code, b.code) })
	return out
}

// messageKey 是代碼在翻譯目錄裡的鍵："agent.version_too_old" → "agent_version_too_old"。
func messageKey(code string) string { return strings.ReplaceAll(code, ".", "_") }

var argRE = regexp.MustCompile(`\{(\w+)\}`)

// TestErrorCatalogs 確認每個代碼在每個語言的翻譯目錄裡都有詞條，詞條的
// 佔位符就是代碼的參數，而且目錄裡沒有已不存在的代碼。
func TestErrorCatalogs(t *testing.T) {
	codes := errorCodes()
	want := map[string]errorCode{}
	var prefixes []string // 各套件的詞條鍵前綴（"agent_"），從代碼得出
	for _, c := range codes {
		if pkg, _, _ := strings.Cut(c.code, "."); !slices.Contains(prefixes, pkg+"_") {
			prefixes = append(prefixes, pkg+"_")
		}
		if _, dup := want[messageKey(c.code)]; dup {
			t.Errorf("two codes share the key %s", messageKey(c.code))
		}
		want[messageKey(c.code)] = c
	}
	files, _ := os.ReadDir("../../locales")
	langs := 0
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		langs++
		t.Run(f.Name(), func(t *testing.T) {
			b, err := os.ReadFile("../../locales/" + f.Name())
			if err != nil {
				t.Fatal(err)
			}
			var catalog map[string]json.RawMessage
			if err := json.Unmarshal(b, &catalog); err != nil {
				t.Fatal(err)
			}
			for key, c := range want {
				var msg string
				if err := json.Unmarshal(catalog[key], &msg); err != nil || msg == "" {
					t.Errorf("%s: no message for %s", f.Name(), c.code)
					continue
				}
				var args []string
				for _, m := range argRE.FindAllStringSubmatch(msg, -1) {
					if !slices.Contains(args, m[1]) {
						args = append(args, m[1])
					}
				}
				if !sameSet(args, c.params) {
					t.Errorf("%s: %s uses {%s}, the code has {%s}", f.Name(), key, strings.Join(args, ", "), strings.Join(c.params, ", "))
				}
				if meta, ok := catalog["@"+key]; ok {
					var m struct {
						Placeholders map[string]json.RawMessage `json:"placeholders"`
					}
					_ = json.Unmarshal(meta, &m)
					if !sameSet(mapKeys(m.Placeholders), c.params) {
						t.Errorf("%s: @%s describes %v, the code has %v", f.Name(), key, mapKeys(m.Placeholders), c.params)
					}
				}
			}
			for key := range catalog {
				isCode := slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(key, p) })
				if _, ok := want[key]; !ok && isCode {
					t.Errorf("%s: %s is no longer a code", f.Name(), key)
				}
			}
		})
	}
	if langs < 2 {
		t.Fatalf("found %d catalogs in locales/", langs)
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func mapKeys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
