package provider

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/bayuncao/oriel/internal/task"
)

// Error 是 provider 給使用者看的錯誤：穩定的代碼加上參數，前端依代碼翻譯；
// Error 回傳的英文只供日誌（docs/architecture/README.md 規則 9）。errors.Is
// 依代碼比對，errors.Unwrap 得到原因。
type Error struct {
	Code   Code              `json:"code"`
	Params map[string]string `json:"params,omitzero"`
	cause  error
}

// Code 是 "provider." 開頭的穩定代碼；意義一經發佈不再改變。
type Code string

const (
	// CodeFetchFailed：一家的狀態頁沒讀成（連不上、超時、不是 200、格式不對）。
	CodeFetchFailed Code = "provider.fetch_failed"
	// CodeClosed：Monitor 已關閉。
	CodeClosed Code = "provider.closed"
	// CodeKeychainUnavailable：系統鑰匙串用不了（沒有憑證儲存，或被拒絕存取，PK09）。
	CodeKeychainUnavailable Code = "provider.keychain_unavailable"
	// CodeKeyNameMissing：鑰匙沒有名稱（PK07）。
	CodeKeyNameMissing Code = "provider.key_name_missing"
	// CodeKeyMissing：沒有貼上鑰匙（PK07）。
	CodeKeyMissing Code = "provider.key_missing"
	// CodeUnknownIcon：圖標不是下拉裡的廠商之一（PK05、PK07）。
	CodeUnknownIcon Code = "provider.unknown_icon"
	// CodeInvalidBaseURL：base URL 不是帶主機名的 http 或 https 地址，或帶了用戶名密碼、查詢串、片段（PK24、PK25）。
	CodeInvalidBaseURL Code = "provider.invalid_base_url"
	// CodeKeyNotFound：要改的那把鑰匙已經不在了（PK32）。
	CodeKeyNotFound Code = "provider.key_not_found"
)

// codeText 是各代碼的英文說明，{name} 由同名參數填入；只供日誌。
// 它的佔位符就是代碼的參數（Code.Params）。
var codeText = map[Code]string{
	CodeFetchFailed:         "reading the {provider} status page failed: {detail}",
	CodeClosed:              "provider monitor is closed",
	CodeKeychainUnavailable: "the system keychain is not available: {detail}",
	CodeKeyNameMissing:      "the key has no name",
	CodeKeyMissing:          "no key was pasted",
	CodeUnknownIcon:         "the key has no provider icon from the list",
	CodeInvalidBaseURL:      "the base URL is not an http or https address with a host, or has a user, query or fragment",
	CodeKeyNotFound:         "the key is not stored anymore",
}

// Codes 是本套件的所有代碼。
func Codes() []Code { return slices.Sorted(maps.Keys(codeText)) }

// Params 是代碼的參數名稱，依說明中出現的順序。
func (c Code) Params() []string { return placeholders(codeText[c]) }

func fetchFailed(provider string, cause error) *Error {
	return &Error{Code: CodeFetchFailed, cause: cause,
		Params: map[string]string{"provider": provider, "detail": cause.Error()}}
}

// keyError 是鑰匙的錯誤；kv 是參數名與值交替。
func keyError(code Code, cause error, kv ...string) *Error {
	params := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		params[kv[i]] = kv[i+1]
	}
	return &Error{Code: code, Params: params, cause: cause}
}

// problem 是錯誤給頁面的形狀。
func (e *Error) problem() task.Problem { return task.Problem{Code: string(e.Code), Params: e.Params} }

var errClosed = &Error{Code: CodeClosed}

func (e *Error) ErrorCode() string              { return string(e.Code) }
func (e *Error) ErrorParams() map[string]string { return e.Params }

func (e *Error) Error() string {
	text := cmp.Or(codeText[e.Code], string(e.Code))
	for k, v := range e.Params {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return "provider: " + text
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
