package locales

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	// 簡單的參數（{agent}）會換成名稱，不算文案；複數與選擇的分支裡是文案，留著檢查。
	argument = regexp.MustCompile(`\{[A-Za-z_][A-Za-z0-9_]*\}`)
	// 中文界面的術語（D119）：Agent、Task、Workspace 寫成智能體、任務、工作區；Session、Registry 等照寫英文。
	untranslated = regexp.MustCompile(`(?i)\b(agents?|tasks?|workspaces?)\b`)
)

// TestChineseTerms 守住 D119：zh-Hans 的文案裡沒有英文的 Agent、Task、Workspace。
func TestChineseTerms(t *testing.T) {
	raw, err := files.ReadFile("zh-Hans.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	var bad []string
	for key, value := range entries {
		if strings.HasPrefix(key, "@") {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			continue
		}
		if w := untranslated.FindString(argument.ReplaceAllString(text, "")); w != "" {
			bad = append(bad, key+" ("+w+"): "+text)
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}
