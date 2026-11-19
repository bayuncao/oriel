package updater

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Error 是 updater 給使用者看的錯誤：穩定的代碼加上參數，前端依代碼
// 翻譯；Error 回傳的英文只供日誌（docs/architecture/README.md 規則 9）。
// errors.Is 依代碼比對，errors.Unwrap 得到原因。
type Error struct {
	Code   Code              `json:"code"`
	Params map[string]string `json:"params,omitzero"`
	cause  error
}

// Code 是 "updater." 開頭的穩定代碼；意義一經發佈不再改變。
type Code string

const (
	CodeCheckFailed   Code = "updater.check_failed"   // detail：問更新源失敗
	CodeInstallFailed Code = "updater.install_failed" // detail：下載、驗簽或替換失敗
)

// codeText 是各代碼的英文說明，{name} 由同名參數填入；只供日誌。
// 它的佔位符就是代碼的參數（Code.Params）。
var codeText = map[Code]string{
	CodeCheckFailed:   "checking for updates failed: {detail}",
	CodeInstallFailed: "installing the update failed: {detail}",
}

// Codes 是本套件的所有代碼。
func Codes() []Code { return slices.Sorted(maps.Keys(codeText)) }

// Params 是代碼的參數名稱，依說明中出現的順序。
func (c Code) Params() []string { return placeholders(codeText[c]) }

// fail 以 cause 的原文作 detail。
func fail(code Code, cause error) *Error {
	return &Error{Code: code, Params: map[string]string{"detail": cause.Error()}, cause: cause}
}

func (e *Error) ErrorCode() string              { return string(e.Code) }
func (e *Error) ErrorParams() map[string]string { return e.Params }

func (e *Error) Error() string {
	text := cmp.Or(codeText[e.Code], string(e.Code))
	for k, v := range e.Params {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return "updater: " + text
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
