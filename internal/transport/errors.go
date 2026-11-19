// Package transport 是各入站適配器共用的部分：把後端的錯誤轉成交給
// 前端的形狀。適配器本身（mygoipc、remote）寫到時才建。
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
)

// Error 是交給前端的錯誤：穩定的代碼加上參數，前端以代碼為翻譯鍵、
// 以參數填空（docs/architecture/README.md 規則 9）。mygo 只把錯誤文字
// 交給頁面，所以 Error() 就是這個形狀的 JSON，頁面以 src/errors.ts
// 的 problemOf 解析。Error 自己也滿足 Coded，所以 Wire 對它不變。
type Error struct {
	Code   string            `json:"code"`
	Params map[string]string `json:"params,omitzero"`
}

func (e *Error) Error() string {
	b, _ := json.Marshal(e)
	return string(b)
}

func (e *Error) ErrorCode() string              { return e.Code }
func (e *Error) ErrorParams() map[string]string { return e.Params }

// Coded 是後端錯誤帶代碼的方式；各套件的錯誤型別自然滿足它，
// 不必匯入本套件。
type Coded interface {
	error
	ErrorCode() string
	ErrorParams() map[string]string
}

// Code 是本套件自己的代碼，都沒有參數。
type Code string

// 沒有代碼的錯誤，以及 context 的取消與逾時。
const (
	CodeInternal Code = "transport.internal"
	CodeCanceled Code = "transport.canceled"
	CodeTimedOut Code = "transport.timed_out"
)

// Codes 是本套件的所有代碼（TestCodes 確認沒有遺漏）。
func Codes() []Code { return []Code{CodeCanceled, CodeInternal, CodeTimedOut} }

// Wire 把 err 轉成交給前端的錯誤，參數是複製的，修改它不影響 err。
// known 為 false 表示 err 沒有代碼（包括代碼為空，以及呼叫者誤傳的
// nil），前端只會看到 CodeInternal：呼叫者應把 err 的原文寫進日誌。
// 結果一律非 nil，免得被當成非 nil 的 error 回傳。
func Wire(err error) (e *Error, known bool) {
	if c, ok := errors.AsType[Coded](err); ok && c.ErrorCode() != "" {
		return &Error{Code: c.ErrorCode(), Params: maps.Clone(c.ErrorParams())}, true
	}
	switch {
	case errors.Is(err, context.Canceled):
		return &Error{Code: string(CodeCanceled)}, true
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: string(CodeTimedOut)}, true
	}
	return &Error{Code: string(CodeInternal)}, false
}
