package mygoipc

// Code 是 mygoipc 的使用者可見錯誤代碼。
type Code string

const (
	// CodeNeedsProfile：頁面不能給 LaunchSnapshot（D57）。
	CodeNeedsProfile Code = "mygoipc.needs_profile"
	// CodeNotReady：app 尚未就緒（mygo generate 時可建構）。
	CodeNotReady Code = "mygoipc.not_ready"
	// CodeUnsupportedLink：Desktop.OpenLink 只開 http、https 與 mailto（m2.md V10）。
	CodeUnsupportedLink Code = "mygoipc.unsupported_link"
)

// Params 回傳代碼的參數名。
func (c Code) Params() []string { return nil }

// Codes 是本套件的所有代碼。
func Codes() []Code { return []Code{CodeNeedsProfile, CodeNotReady, CodeUnsupportedLink} }

// codeError 是帶代碼的錯誤（無參數）。
type codeError struct{ code Code }

func (e codeError) Error() string                  { return string(e.code) }
func (e codeError) ErrorCode() string              { return string(e.code) }
func (e codeError) ErrorParams() map[string]string { return nil }

var (
	errNotReady        = codeError{CodeNotReady}
	errUnsupportedLink = codeError{CodeUnsupportedLink}
)
