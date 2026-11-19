package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/store"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
)

func ctxb2() context.Context { return context.Background() }

// N1：七種 Kind 在 en 與 zh-Hans 都渲染出非空標題與內文；
// 缺 Params 也不出事。
func TestRenderAllKinds(t *testing.T) {
	for _, k := range []hub.NotificationKind{
		hub.NotifyAsk, hub.NotifyFailed, hub.NotifyStalled, hub.NotifyRecommended,
		hub.NotifyConfirmation, hub.NotifyConflict, hub.NotifyCompleted} {
		n := hub.Notification{Kind: k, TaskTitle: "t",
			Params: map[string]string{
				"agent": "grok", "tool": "Bash", "kind": "merge",
				"files": "3", "command": "rm -rf /", "minutes": "10"}}
		for _, loc := range []string{"en", "zh-Hans"} {
			title, body, _ := render(n, loc)
			if title == "" || body == "" {
				t.Fatalf("%s %s = %q %q", k, loc, title, body)
			}
		}
		// 無 Params
		title, body, _ := render(hub.Notification{Kind: k}, "en")
		if title == "" {
			t.Fatalf("empty title %s", k)
		}
		_ = body
	}
}

// ST01、ST03、ST15：沒有動靜的通知寫 agent 與分鐘數。
func TestRenderStalled(t *testing.T) {
	for _, tt := range []struct{ loc, minutes, want string }{
		{"zh-Hans", "10", "Grok 已有 10 分钟没有动静"},
		{"zh-Hans", "30", "Grok 已有 30 分钟没有动静"},
		{"en", "10", "Grok has had no activity for 10 minutes"},
	} {
		n := hub.Notification{Kind: hub.NotifyStalled, TaskTitle: "fix login",
			Params: map[string]string{"agent": "Grok", "minutes": tt.minutes}}
		title, body, _ := render(n, tt.loc)
		if title != "fix login" || body != tt.want {
			t.Errorf("%s %s: %q %q, want %q", tt.loc, tt.minutes, title, body, tt.want)
		}
	}
}

// F1–F5：回合失敗的通知正文帶上原因的譯文；目錄裡沒有的代碼只寫“回合失敗”，不露出代碼。
func TestRenderFailedReason(t *testing.T) {
	cases := []struct {
		name   string
		reason *task.Problem
		locale string
		want   string
	}{
		{"F1 agent exited", &task.Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}, "zh-Hans", "回合失败：程序以状态 1 退出"},
		{"F2 reason with detail", &task.Problem{Code: "acp.prompt_failed", Params: map[string]string{"detail": "rate limited"}}, "zh-Hans", "回合失败：智能体没能完成这一轮：rate limited"},
		{"F3 reason without params", &task.Problem{Code: "acp.refusal"}, "zh-Hans", "回合失败：智能体拒绝继续"},
		{"F4 unknown code", &task.Problem{Code: "someone.else_failed"}, "zh-Hans", "回合失败"},
		{"F4 no reason", nil, "zh-Hans", "回合失败"},
		{"F5 English", &task.Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}, "en", "A turn failed: The program exited with status 1"},
		{"missing param is empty, not a placeholder", &task.Problem{Code: "acp.prompt_failed"}, "zh-Hans", "回合失败：智能体没能完成这一轮："},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, body, unknown := render(hub.Notification{Kind: hub.NotifyFailed, TaskTitle: "t", Reason: c.reason}, c.locale)
			if body != c.want {
				t.Fatalf("body = %q, want %q", body, c.want)
			}
			wantUnknown := ""
			if c.reason != nil && c.want == "回合失败" {
				wantUnknown = c.reason.Code
			}
			if unknown != wantUnknown {
				t.Fatalf("unknown = %q, want %q", unknown, wantUnknown)
			}
		})
	}
}

// 原因只用在回合失敗：其他種類即使帶了 Reason，正文仍是自己的。
func TestRenderReasonOnlyForFailed(t *testing.T) {
	_, body, unknown := render(hub.Notification{Kind: hub.NotifyCompleted, Reason: &task.Problem{Code: "proc.exited", Params: map[string]string{"status": "1"}}}, "zh-Hans")
	if body != "回合已完成" || unknown != "" {
		t.Fatalf("body = %q, unknown = %q", body, unknown)
	}
}

// N2：ID 往返；格式錯的拒絕。
func TestTaskOfNotification(t *testing.T) {
	n := hub.Notification{Task: "t-abc", Kind: hub.NotifyAsk, Session: "s-1"}
	id := notificationID(n)
	got, ok := TaskOfNotification(id)
	if !ok || got != n.Task {
		t.Fatalf("%q → %v %v", id, got, ok)
	}
	for _, bad := range []string{"", "nosep", "/kind/s", "t-x//"} {
		if _, ok := TaskOfNotification(bad); ok && bad != "t-x//" {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// A6：renderError 有代碼走目錄；沒代碼包 app.start_failed。
func TestRenderError(t *testing.T) {
	e, known := transport.Wire(storeErr(t))
	_ = known
	s := renderError(storeErr(t), "en")
	if !strings.Contains(s, "99") {
		t.Fatalf("%q (code %s)", s, e.Code)
	}
	if s := renderError(errors.New("disk on fire"), "en"); !strings.Contains(s, "disk on fire") {
		t.Fatalf("%q", s)
	}
	// 有代碼但目錄裡沒有：包成 app.start_failed，不露出代碼當作文字。
	if s := renderError(codedErr{code: "someone.else_failed"}, "en"); s == "someone_else_failed" || !strings.Contains(s, "boom") {
		t.Fatalf("%q", s)
	}
}

// codedErr 是帶代碼的錯誤（實作 ErrorCode / ErrorParams），代碼由測試給。
type codedErr struct{ code string }

func (e codedErr) Error() string                  { return "boom" }
func (e codedErr) ErrorCode() string              { return e.code }
func (e codedErr) ErrorParams() map[string]string { return nil }

func storeErr(t *testing.T) error {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.db")
	db, err := store.Open(ctxb2(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	bumpUserVersion(t, path, 99)
	_, err = store.Open(ctxb2(), path, store.Options{})
	if err == nil {
		t.Fatal("want newer_schema")
	}
	return err
}

// bumpUserVersion 直接改檔案頭的 user_version（offset 60，
// 大端 uint32）；internal/app 不能匯入 sqlite 外掛。
func bumpUserVersion(t *testing.T, path string, v uint32) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 64 {
		t.Fatal(err)
	}
	b[60] = byte(v >> 24)
	b[61] = byte(v >> 16)
	b[62] = byte(v >> 8)
	b[63] = byte(v)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
