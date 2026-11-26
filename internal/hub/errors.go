package hub

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Error 是 hub 給使用者看的錯誤：穩定的代碼加上參數，前端依代碼
// 翻譯；Error 回傳的英文只供日誌（docs/architecture/README.md 規則 9）。
// errors.Is 依代碼比對，errors.Unwrap 得到原因。
type Error struct {
	Code   Code              `json:"code"`
	Params map[string]string `json:"params,omitzero"`
	cause  error
}

// Code 是 "hub." 開頭的穩定代碼；意義一經發佈不再改變。
type Code string

const (
	CodeUnknownTask      Code = "hub.unknown_task"      //
	CodeUnknownWorkspace Code = "hub.unknown_workspace" // path
	CodeClosed           Code = "hub.closed"            //
	CodeNoRuntime        Code = "hub.no_runtime"        // protocol
	CodeSecretMissing    Code = "hub.secret_missing"    // name
	CodeConnectionLost   Code = "hub.connection_lost"   //
	CodeEffectFailed     Code = "hub.effect_failed"     // detail
	CodeUnsupported      Code = "hub.unsupported"       // op
	CodeStoreFailed      Code = "hub.store_failed"      // detail
	CodeUnknownMessage   Code = "hub.unknown_message"   // message
	CodeLagging          Code = "hub.lagging"           //
	CodeNeedsDesktop     Code = "hub.needs_desktop"     //
	CodeUnknownRule      Code = "hub.unknown_rule"      // rule
	CodeInvalidField     Code = "hub.invalid_field"     // field
	CodeAgentUnsupported Code = "hub.agent_unsupported" // agent
	CodeUnknownLabel     Code = "hub.unknown_label"     // label
	CodeDuplicateLabel   Code = "hub.duplicate_label"   // name
	CodeNotArchived      Code = "hub.not_archived"      //
	CodeUnknownDevice    Code = "hub.unknown_device"    // device
	CodeNoWorktree       Code = "hub.no_worktree"       //
)

// ErrClosed 是 hub 關閉後的操作錯誤。
var ErrClosed = &Error{Code: CodeClosed}

// codeText 是各代碼的英文說明，{name} 由同名參數填入；只供日誌。
var codeText = map[Code]string{
	CodeUnknownTask:      "no such task",
	CodeUnknownWorkspace: "workspace {path} is not registered",
	CodeClosed:           "hub is closed",
	CodeNoRuntime:        "no runtime for protocol {protocol}",
	CodeSecretMissing:    "secret {name} is not available",
	CodeConnectionLost:   "the connection was lost",
	CodeEffectFailed:     "effect failed: {detail}",
	CodeUnsupported:      "{op} is not supported yet",
	CodeStoreFailed:      "store failed: {detail}",
	CodeUnknownMessage:   "message {message} is unknown",
	CodeLagging:          "subscriber fell behind",
	CodeNeedsDesktop:     "operation requires the desktop",
	CodeUnknownRule:      "rule {rule} is unknown",
	CodeInvalidField:     "field {field} is invalid",
	CodeAgentUnsupported: "{agent} cannot be used from OrielHub yet",
	CodeUnknownLabel:     "label {label} is unknown",
	CodeDuplicateLabel:   "a label named {name} already exists",
	CodeNotArchived:      "only an archived task can be deleted",
	CodeUnknownDevice:    "device {device} is not paired",
	CodeNoWorktree:       "the task has no open worktree",
}

// Codes 是本套件的所有代碼。
func Codes() []Code { return slices.Sorted(maps.Keys(codeText)) }

// Params 是代碼的參數名稱，依說明中出現的順序。
func (c Code) Params() []string { return placeholders(codeText[c]) }

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
	return "hub: " + text
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
