package claude

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Error 是 Claude 轉接器給使用者看的錯誤：穩定的代碼加參數，前端依代碼翻譯；
// Error 回傳的英文只供日誌（docs/architecture/README.md 規則 9）。
type Error struct {
	Code   Code
	Params map[string]string
	cause  error
}

// Code 是 "claude." 開頭的穩定代碼；意義一經發佈不再改變。
type Code string

const (
	CodeStartTimedOut         Code = "claude.start_timed_out"        //
	CodePermissionMode        Code = "claude.permission_mode"        // mode
	CodeRequestFailed         Code = "claude.request_failed"         // request, detail
	CodeTurnFailed            Code = "claude.turn_failed"            // detail
	CodeMaxTurns              Code = "claude.max_turns"              //
	CodeExecutionError        Code = "claude.execution_error"        // detail
	CodeAttachmentUnsupported Code = "claude.attachment_unsupported" // name
	CodeProtocolError         Code = "claude.protocol_error"         // detail
	CodeSetModelTimedOut      Code = "claude.set_model_timed_out"    //
)

// codeText 是各代碼的英文說明，{name} 由同名參數填入；只供日誌。
var codeText = map[Code]string{
	CodeStartTimedOut:         "Claude Code did not start in time",
	CodePermissionMode:        "Claude Code started in permission mode {mode}, not the one that asks OrielHub",
	CodeRequestFailed:         "Claude Code refused {request}: {detail}",
	CodeTurnFailed:            "the turn failed: {detail}",
	CodeMaxTurns:              "Claude Code reached its turn limit",
	CodeExecutionError:        "Claude Code stopped with an error: {detail}",
	CodeAttachmentUnsupported: "{name} cannot be attached yet",
	CodeProtocolError:         "Claude Code broke the protocol: {detail}",
	CodeSetModelTimedOut:      "Claude Code did not switch models in time",
}

// Codes 是本套件的所有代碼。
func Codes() []Code { return slices.Sorted(maps.Keys(codeText)) }

// Params 是代碼的參數名稱，依說明中出現的順序。
func (c Code) Params() []string {
	var out []string
	for _, m := range placeholderRE.FindAllStringSubmatch(codeText[c], -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

var placeholderRE = regexp.MustCompile(`\{([a-z_]+)\}`)

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
	return "claude: " + text
}

func (e *Error) Unwrap() error { return e.cause }

// Is 讓同代碼的 Error 彼此相等，不論參數。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// exitError 是 Claude Code 行程已結束時 Start 回的錯誤（hub.ExitError，D102）：
// 原因是行程的結束原因（proc.exited / proc.signaled）或協議錯誤，附 stderr 尾部。
type exitError struct {
	code   string
	params map[string]string
	tail   string
}

func (e *exitError) Error() string {
	return "claude: Claude Code exited before the session started: " + e.code
}
func (e *exitError) ErrorCode() string              { return e.code }
func (e *exitError) ErrorParams() map[string]string { return e.params }
func (e *exitError) StderrTail() string             { return e.tail }
