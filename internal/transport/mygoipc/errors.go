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
	// CodeRemoteUnavailable：這個構建沒有遠程（正式版，D149，remote.md DV18），Remote 的方法立即回它。
	CodeRemoteUnavailable Code = "mygoipc.remote_unavailable"
	// CodeUnknownFolder：Desktop.RevealFolder 只打開資料與日誌目錄（settings.md PV05）。
	CodeUnknownFolder Code = "mygoipc.unknown_folder"
)

// Params 回傳代碼的參數名。
func (c Code) Params() []string { return nil }

// Codes 是本套件的所有代碼。
func Codes() []Code {
	return []Code{CodeNeedsProfile, CodeNotReady, CodeUnsupportedLink, CodeRemoteUnavailable, CodeUnknownFolder}
}

// codeError 是帶代碼的錯誤（無參數）。
type codeError struct{ code Code }

func (e codeError) Error() string                  { return string(e.code) }
func (e codeError) ErrorCode() string              { return string(e.code) }
func (e codeError) ErrorParams() map[string]string { return nil }

var (
	errNotReady          = codeError{CodeNotReady}
	errUnsupportedLink   = codeError{CodeUnsupportedLink}
	errRemoteUnavailable = codeError{CodeRemoteUnavailable}
	errUnknownFolder     = codeError{CodeUnknownFolder}
)
