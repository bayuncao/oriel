package store

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Code 是 store 的錯誤代碼；文字只是給日誌與 {placeholder} 清單用的。
type Code string

const (
	CodeOpenFailed  Code = "store.open_failed"  // detail
	CodeNewerSchema Code = "store.newer_schema" // version
	CodeConflict    Code = "store.conflict"     // task
	CodeClosed      Code = "store.closed"       //
	CodeFailed      Code = "store.failed"       // detail
)

var codeText = map[Code]string{
	CodeOpenFailed:  "cannot open database: {detail}",
	CodeNewerSchema: "database schema version {version} is newer than this app",
	CodeConflict:    "concurrent write to task {task}",
	CodeClosed:      "store is closed",
	CodeFailed:      "database operation failed: {detail}",
}

// Codes 回傳所有代碼（已排序）。
func Codes() []Code { return slices.Sorted(maps.Keys(codeText)) }

var paramRE = regexp.MustCompile(`\{(\w+)\}`)

// Params 從英文文字解析出參數名稱。
func (c Code) Params() []string {
	var out []string
	for _, m := range paramRE.FindAllStringSubmatch(codeText[c], -1) {
		out = append(out, m[1])
	}
	return out
}

// Error 是帶代碼與參數的錯誤。
type Error struct {
	Code   Code
	Params map[string]string
	cause  error
}

func fail(code Code, cause error, kv ...string) *Error {
	e := &Error{Code: code, cause: cause}
	if len(kv) > 0 {
		e.Params = map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			e.Params[kv[i]] = kv[i+1]
		}
	}
	return e
}

func (e *Error) Error() string {
	text := codeText[e.Code]
	if text == "" {
		text = string(e.Code)
	}
	for k, v := range e.Params {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", text, e.cause)
	}
	return text
}

func (e *Error) Unwrap() error { return e.cause }

func (e *Error) ErrorCode() string { return string(e.Code) }

func (e *Error) ErrorParams() map[string]string { return e.Params }

// Is 依代碼比較。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}
