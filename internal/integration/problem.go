package integration

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/bayuncao/oriel/internal/task"
)

// Error 是集成給使用者看的錯誤：穩定的代碼加上參數，前端依代碼翻譯；
// Error 回傳的英文只供日誌（docs/architecture/README.md 規則 9）。errors.Is
// 依代碼比對，errors.Unwrap 得到原因。
type Error struct {
	Code   Code              `json:"code"`
	Params map[string]string `json:"params,omitzero"`
	cause  error
}

// Code 是 "integration." 開頭的穩定代碼；意義一經發佈不再改變。
type Code string

const (
	// CodeGhMissing：登入 shell 的 PATH 與使用者工具目錄裡都沒有 gh（IG01）。
	CodeGhMissing Code = "integration.gh_missing"
	// CodeGhLoggedOut：gh 沒有登入 github.com（沒有作用中的帳號，IG02）。
	CodeGhLoggedOut Code = "integration.gh_logged_out"
	// CodeGhScopeMissing：gh 的 token 少了需要的 scope（IG03）。
	CodeGhScopeMissing Code = "integration.gh_scope_missing"
	// CodeGhUnreachable：gh 沒能向 GitHub 確認登入（逾時、連不上，或 token 失效；detail 是 gh 說的原因，IG16）。
	CodeGhUnreachable Code = "integration.gh_unreachable"
	// CodeGhFailed：gh 沒有給出看得懂的回答：超時、非 0 結束、輸出不是 JSON（IG10、IG11）。
	CodeGhFailed Code = "integration.gh_failed"
	// CodeUnknown：沒有這個集成（IG09）。
	CodeUnknown Code = "integration.unknown"
	// CodeClosed：Service 已關閉。
	CodeClosed Code = "integration.closed"
)

// codeText 是各代碼的英文說明，{name} 由同名參數填入；只供日誌。
// 它的佔位符就是代碼的參數（Code.Params）。
var codeText = map[Code]string{
	CodeGhMissing:      "the GitHub CLI (gh) is not installed",
	CodeGhLoggedOut:    "gh is not logged in to github.com",
	CodeGhScopeMissing: "the gh token lacks the {scope} scope",
	CodeGhUnreachable:  "gh could not confirm the sign-in with GitHub: {detail}",
	CodeGhFailed:       "checking gh failed: {detail}",
	CodeUnknown:        "there is no integration {id}",
	CodeClosed:         "the integration service is closed",
}

// Codes 是本套件的所有代碼。
func Codes() []Code { return slices.Sorted(maps.Keys(codeText)) }

// Params 是代碼的參數名稱，依說明中出現的順序。
func (c Code) Params() []string { return placeholders(codeText[c]) }

// fail 建立錯誤；kv 是參數名與值交替。
func fail(code Code, cause error, kv ...string) *Error {
	var params map[string]string
	if len(kv) > 0 {
		params = map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			params[kv[i]] = kv[i+1]
		}
	}
	return &Error{Code: code, Params: params, cause: cause}
}

// problem 是錯誤給頁面的形狀。
func (e *Error) problem() *task.Problem {
	return &task.Problem{Code: string(e.Code), Params: maps.Clone(e.Params)}
}

var errClosed = &Error{Code: CodeClosed}

func (e *Error) ErrorCode() string              { return string(e.Code) }
func (e *Error) ErrorParams() map[string]string { return e.Params }

func (e *Error) Error() string {
	text := cmp.Or(codeText[e.Code], string(e.Code))
	for k, v := range e.Params {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return "integration: " + text
}

func (e *Error) Unwrap() error { return e.cause }

// Is 讓同代碼的 Error 彼此相等，不論參數。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

var placeholderRE = regexp.MustCompile(`\{([a-z_]+)\}`)

func placeholders(text string) []string {
	var out []string
	for _, m := range placeholderRE.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}
