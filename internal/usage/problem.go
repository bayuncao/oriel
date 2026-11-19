package usage

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Error 是 usage 給使用者看的錯誤：穩定的代碼加上參數，前端依代碼翻譯；
// Error 回傳的英文只供日誌（docs/architecture/README.md 規則 9）。errors.Is
// 依代碼比對，errors.Unwrap 得到原因。
type Error struct {
	Code   Code              `json:"code"`
	Params map[string]string `json:"params,omitzero"`
	cause  error
}

// Code 是 "usage." 開頭的穩定代碼；意義一經發佈不再改變。
type Code string

const (
	// CodeReadFailed：一個位置或記錄檔讀不了，原因顯示在記錄來源裡。
	CodeReadFailed Code = "usage.read_failed"
	// CodeClosed：Tracker 已關閉。
	CodeClosed Code = "usage.closed"
)

// codeText 是各代碼的英文說明，{name} 由同名參數填入；只供日誌。
// 它的佔位符就是代碼的參數（Code.Params）。
var codeText = map[Code]string{
	CodeReadFailed: "reading the {agent} usage records at {path} failed: {detail}",
	CodeClosed:     "usage tracker is closed",
}

// Codes 是本套件的所有代碼。
func Codes() []Code { return slices.Sorted(maps.Keys(codeText)) }

// Params 是代碼的參數名稱，依說明中出現的順序。
func (c Code) Params() []string { return placeholders(codeText[c]) }

func readFailed(agent, path string, cause error) *Error {
	return &Error{Code: CodeReadFailed, cause: cause,
		Params: map[string]string{"agent": agent, "path": path, "detail": cause.Error()}}
}

var errClosed = &Error{Code: CodeClosed}

func (e *Error) ErrorCode() string              { return string(e.Code) }
func (e *Error) ErrorParams() map[string]string { return e.Params }

func (e *Error) Error() string {
	text := cmp.Or(codeText[e.Code], string(e.Code))
	for k, v := range e.Params {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return "usage: " + text
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
