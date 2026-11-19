package app

import (
	"strconv"
	"strings"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
	"github.com/bayuncao/oriel/locales"
)

// render 產生通知的標題與內文（文字由 locales 渲染，D55）。回合失敗帶原因時正文是“回合失敗：原因”（m2.md N28–N32）；
// 原因的代碼目錄裡沒有時正文只寫“回合失敗”，unknown 是那個代碼，交給調用處記日誌。
func render(n hub.Notification, locale string) (title, body, unknown string) {
	title = n.TaskTitle
	if title == "" {
		title = "OrielHub"
	}
	if n.Kind == hub.NotifyFailed && n.Reason != nil {
		if reason, ok := problemText(n.Reason.Code, n.Reason.Params, locale); ok {
			return title, locales.Catalog.Format(locale, "notify_failed_reason", map[string]any{"reason": reason}), ""
		}
		unknown = n.Reason.Code
	}
	args := map[string]any{}
	for k, v := range n.Params {
		args[k] = v
	}
	what := n.Params["command"]
	if what == "" {
		what = n.Params["title"]
	}
	if what == "" {
		what = n.Params["tool"]
	}
	args["what"] = what
	// 複數規則的參數要整數。
	for _, k := range []string{"files", "minutes"} {
		if v, err := strconv.Atoi(n.Params[k]); err == nil {
			args[k] = v
		}
	}
	body = locales.Catalog.Format(locale,
		"notify_"+string(n.Kind), args)
	return title, body, unknown
}

// problemText 把一個帶參數的代碼（錯誤或原因）譯成界面語言：代碼的 . 換成 _ 即目錄的鍵，參數照填，
// 訊息要的參數沒給時填空字串（同前端的 describe），不露出 {detail} 這樣的佔位。代碼為空或目錄裡沒有時回 false。
func problemText(code string, params map[string]string, locale string) (string, bool) {
	if code == "" {
		return "", false
	}
	key := strings.ReplaceAll(code, ".", "_")
	m, _, ok := locales.Catalog.Message(locale, key)
	if !ok {
		return "", false
	}
	args := map[string]any{}
	for _, a := range m.Args() {
		args[a.Name] = ""
	}
	for k, v := range params {
		args[k] = v
	}
	return locales.Catalog.Format(locale, key, args), true
}

// notificationID 是通知點擊回傳的 ID：<task>/<kind>/<session>。
func notificationID(n hub.Notification) string {
	return string(n.Task) + "/" + string(n.Kind) + "/" + string(n.Session)
}

// TaskOfNotification 從通知 ID 解析 TaskID；格式不對回傳 false。
func TaskOfNotification(id string) (task.TaskID, bool) {
	taskID, _, ok := strings.Cut(id, "/")
	if !ok || taskID == "" {
		return "", false
	}
	return task.TaskID(taskID), true
}

// renderError 給啟動錯誤對話框用：有代碼且目錄裡有它時走 locales，
// 否則包成 app.start_failed，帶原始錯誤的文字。
func renderError(err error, locale string) string {
	if e, known := transport.Wire(err); known {
		if text, ok := problemText(e.Code, e.Params, locale); ok {
			return text
		}
	}
	return locales.Catalog.Format(locale, "app_start_failed",
		map[string]any{"detail": err.Error()})
}
