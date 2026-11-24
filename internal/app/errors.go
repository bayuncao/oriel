package app

// errorCode 的定義沿用 workspace/errors.go 的型別：每個包管自己的代碼。

// Code 是 app 的使用者可見錯誤代碼。
type Code string

// Params 回傳代碼的參數名（給錯誤表測試用）。
func (c Code) Params() []string {
	switch c {
	case CodeStartFailed, CodeLanguageSaveFailed, CodeKeepAwakeSaveFailed, CodeThemeSaveFailed:
		return []string{"detail"}
	case CodeUnknownLanguage:
		return []string{"tag"}
	case CodeUnknownTheme:
		return []string{"theme"}
	}
	return nil
}

const (
	CodeStartFailed        Code = "app.start_failed"         // detail
	CodeUnknownLanguage    Code = "app.unknown_language"     // tag：不是 app 寫成的語言
	CodeLanguageSaveFailed Code = "app.language_save_failed" // detail：記不住語言選擇
	// CodeKeepAwakeSaveFailed：記不住“Agent 運行時防止電腦睡眠”的開關（D80）；detail
	CodeKeepAwakeSaveFailed Code = "app.keep_awake_save_failed"
	// CodeUnknownTheme：外觀不是 light、dark 或 ""（跟隨系統）；theme
	CodeUnknownTheme Code = "app.unknown_theme"
	// CodeThemeSaveFailed：記不住外觀（這次照樣生效，PF08）；detail
	CodeThemeSaveFailed Code = "app.theme_save_failed"
)

// Codes 是本套件的所有代碼。
func Codes() []Code {
	return []Code{CodeStartFailed, CodeUnknownLanguage, CodeLanguageSaveFailed, CodeKeepAwakeSaveFailed, CodeUnknownTheme, CodeThemeSaveFailed}
}

// codedError 讓 err 帶上使用者可見代碼。
type codedError struct {
	code   Code
	err    error
	params map[string]string
}

func (e *codedError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return string(e.code)
}
func (e *codedError) Unwrap() error                  { return e.err }
func (e *codedError) ErrorCode() string              { return string(e.code) }
func (e *codedError) ErrorParams() map[string]string { return e.params }

func coded(c Code, err error, kv ...string) *codedError {
	e := &codedError{code: c, err: err}
	for i := 0; i+1 < len(kv); i += 2 {
		if e.params == nil {
			e.params = map[string]string{}
		}
		e.params[kv[i]] = kv[i+1]
	}
	return e
}
