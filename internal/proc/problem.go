package proc

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Error 是 proc 給使用者看的錯誤：穩定的代碼加上參數，前端依代碼
// 翻譯；Error 回傳的英文只供日誌（docs/architecture/README.md 規則 9）。
// errors.Is 依代碼比對，errors.Unwrap 得到原因（如 context.DeadlineExceeded）。
type Error struct {
	Code   Code              `json:"code"`
	Params map[string]string `json:"params,omitzero"`
	cause  error
}

// Code 是 "proc." 開頭的穩定代碼；意義一經發佈不再改變。
type Code string

const (
	CodeClosed      Code = "proc.closed"       //
	CodeNoShell     Code = "proc.no_shell"     //
	CodeStartFailed Code = "proc.start_failed" // path, detail
	CodeExited      Code = "proc.exited"       // status
	CodeSignaled    Code = "proc.signaled"     // signal
	CodeTimedOut    Code = "proc.timed_out"    //
	CodeCanceled    Code = "proc.canceled"     //
)

// codeText 是各代碼的英文說明，{name} 由同名參數填入；只供日誌。
// 它的佔位符就是代碼的參數（Code.Params）。
var codeText = map[Code]string{
	CodeClosed:      "runner closed",
	CodeNoShell:     "no shell answered",
	CodeStartFailed: "cannot start {path}: {detail}",
	CodeExited:      "exited with status {status}",
	CodeSignaled:    "killed by signal {signal}",
	CodeTimedOut:    "timed out",
	CodeCanceled:    "canceled",
}

// Codes 是本套件的所有代碼。
func Codes() []Code { return slices.Sorted(maps.Keys(codeText)) }

// Params 是代碼的參數名稱，依說明中出現的順序。
func (c Code) Params() []string { return placeholders(codeText[c]) }

var (
	// ErrClosed 是被 Runner.Close 停止或在它之後啟動的探測的錯誤。
	ErrClosed = &Error{Code: CodeClosed}
	// ErrNoShell 表示沒有 shell 回應；呼叫者保留繼承的環境。
	ErrNoShell = &Error{Code: CodeNoShell}
)

func fail(code Code, cause error, params ...string) *Error {
	e := &Error{Code: code, cause: cause}
	if len(params) > 0 {
		e.Params = make(map[string]string, len(params)/2)
		for i := 0; i+1 < len(params); i += 2 {
			e.Params[params[i]] = params[i+1]
		}
	}
	return e
}

func (e *Error) ErrorCode() string              { return string(e.Code) }
func (e *Error) ErrorParams() map[string]string { return e.Params }

func (e *Error) Error() string {
	text := cmp.Or(codeText[e.Code], string(e.Code))
	for k, v := range e.Params {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return "proc: " + text
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
