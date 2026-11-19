package remote

// Code 是 remote 的使用者可見錯誤代碼；參數名見 Params。
type Code string

const (
	// CodePairingExpired：配對碼過了 5 分鐘、被新的碼取代，或桌面關掉了二維碼（PA04、PA06）。
	CodePairingExpired Code = "remote.pairing_expired"
	// CodePairingUsed：配對碼已被一台 App 用過（PA05）。
	CodePairingUsed Code = "remote.pairing_used"
	// CodeUnknownDevice：這支手機沒有配對，或已被撤銷（CN02、CN03、CN05）。
	CodeUnknownDevice Code = "remote.unknown_device"
	// CodeNotAllowed：方法不在遠程的白名單裡（CN07、PA09）。
	CodeNotAllowed Code = "remote.not_allowed"
	// CodeBadRequest：請求的參數解不開，或 id 與進行中的訂閱重複。
	CodeBadRequest Code = "remote.bad_request"
	// CodeKeychainUnavailable：讀寫鑰匙串裡的 Mac 密鑰或房間密鑰失敗（DV11）；detail。
	CodeKeychainUnavailable Code = "remote.keychain_unavailable"
	// CodeBadRelayURL：中繼地址不是 ws:// 或 wss:// 開頭的地址（DV15）；url。
	CodeBadRelayURL Code = "remote.bad_relay_url"
	// CodeNotConnected：還沒連上中繼，不能顯示配對碼（DV09）。
	CodeNotConnected Code = "remote.not_connected"
	// CodeSaveFailed：記不住遠程的開關或中繼地址（DV12）；detail。
	CodeSaveFailed Code = "remote.save_failed"
)

// Params 回傳代碼的參數名。
func (c Code) Params() []string {
	switch c {
	case CodeKeychainUnavailable, CodeSaveFailed:
		return []string{"detail"}
	case CodeBadRelayURL:
		return []string{"url"}
	}
	return nil
}

// Codes 是本套件的所有代碼。
func Codes() []Code {
	return []Code{CodeBadRelayURL, CodeBadRequest, CodeKeychainUnavailable, CodeNotAllowed, CodeNotConnected,
		CodePairingExpired, CodePairingUsed, CodeSaveFailed, CodeUnknownDevice}
}

// codeError 是帶代碼的錯誤（無參數）。
type codeError struct{ code Code }

func (e codeError) Error() string                  { return "remote: " + string(e.code) }
func (e codeError) ErrorCode() string              { return string(e.code) }
func (e codeError) ErrorParams() map[string]string { return nil }

// paramError 是帶代碼與參數的錯誤，保留原因（日誌用）。
type paramError struct {
	code   Code
	params map[string]string
	cause  error
}

func (e *paramError) Error() string {
	if e.cause != nil {
		return "remote: " + string(e.code) + ": " + e.cause.Error()
	}
	return "remote: " + string(e.code)
}
func (e *paramError) Unwrap() error                  { return e.cause }
func (e *paramError) ErrorCode() string              { return string(e.code) }
func (e *paramError) ErrorParams() map[string]string { return e.params }

func keychainError(err error) error {
	return &paramError{code: CodeKeychainUnavailable, params: map[string]string{"detail": err.Error()}, cause: err}
}

func saveError(err error) error {
	return &paramError{code: CodeSaveFailed, params: map[string]string{"detail": err.Error()}, cause: err}
}

var (
	errNotConnected   = codeError{CodeNotConnected}
	errPairingExpired = codeError{CodePairingExpired}
	errPairingUsed    = codeError{CodePairingUsed}
	errUnknownDevice  = codeError{CodeUnknownDevice}
	errNotAllowed     = codeError{CodeNotAllowed}
	errBadRequest     = codeError{CodeBadRequest}
)
