package acp

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Error 是 ACP 轉接器給使用者看的錯誤：穩定的代碼加參數，前端依代碼
// 翻譯；Error 回傳的英文只供日誌（docs/architecture/README.md 規則 9）。
type Error struct {
	Code   Code
	Params map[string]string
	cause  error
}

// Code 是 "acp." 開頭的穩定代碼；意義一經發佈不再改變。
type Code string

const (
	CodeProtocolVersion       Code = "acp.protocol_version"       // version
	CodeAuthRequired          Code = "acp.auth_required"          // agent
	CodeStartTimedOut         Code = "acp.start_timed_out"        //
	CodeResumeUnsupported     Code = "acp.resume_unsupported"     //
	CodeRequestFailed         Code = "acp.request_failed"         // method, detail
	CodePromptFailed          Code = "acp.prompt_failed"          // detail
	CodeMaxTokens             Code = "acp.max_tokens"             //
	CodeMaxTurnRequests       Code = "acp.max_turn_requests"      //
	CodeRefusal               Code = "acp.refusal"                //
	CodeAttachmentUnsupported Code = "acp.attachment_unsupported" // name
	CodeProtocolError         Code = "acp.protocol_error"         // detail
	CodeOpenCodeConfig        Code = "acp.opencode_config"        // detail
	CodeSetModelTimedOut      Code = "acp.set_model_timed_out"    //
)

// codeText 是各代碼的英文說明，{name} 由同名參數填入；只供日誌。
var codeText = map[Code]string{
	CodeProtocolVersion:       "the agent speaks ACP version {version}, OrielHub speaks 1",
	CodeAuthRequired:          "{agent} needs you to log in first",
	CodeStartTimedOut:         "the agent did not start a session in time",
	CodeResumeUnsupported:     "the agent cannot resume sessions",
	CodeRequestFailed:         "{method} failed: {detail}",
	CodePromptFailed:          "the prompt failed: {detail}",
	CodeMaxTokens:             "the agent reached its token limit",
	CodeMaxTurnRequests:       "the agent reached its request limit for one turn",
	CodeRefusal:               "the agent refused to continue",
	CodeAttachmentUnsupported: "{name} cannot be attached yet",
	CodeProtocolError:         "the agent broke the protocol: {detail}",
	CodeOpenCodeConfig:        "OPENCODE_CONFIG_CONTENT is not a JSON object: {detail}",
	CodeSetModelTimedOut:      "the agent did not switch models in time",
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
	return "acp: " + text
}

func (e *Error) Unwrap() error { return e.cause }

// Is 讓同代碼的 Error 彼此相等，不論參數。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// exitError 是 agent 行程已結束時 Start 回的錯誤（hub.ExitError，D102）：
// 原因是行程的結束原因（proc.exited / proc.signaled）或協議錯誤，附 stderr 尾部。
type exitError struct {
	code   string
	params map[string]string
	tail   string
}

func (e *exitError) Error() string                  { return "acp: agent exited before the session started: " + e.code }
func (e *exitError) ErrorCode() string              { return e.code }
func (e *exitError) ErrorParams() map[string]string { return e.params }
func (e *exitError) StderrTail() string             { return e.tail }
