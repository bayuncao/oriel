package mygoipc

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/integration"
	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
	"github.com/bayuncao/oriel/internal/workspace"
)

func testHub(t *testing.T) *hub.Hub {
	t.Helper()
	ms := hub.NewMemStore()
	h := hub.New(hub.Options{
		Log: ms, Config: ms, Index: ms,
		Runtimes: map[task.Protocol]hub.Runtime{},
		Git: workspace.New(workspace.Options{
			Runner: &proc.Runner{}, Env: func(context.Context) (proc.Env, error) {
				return proc.Inherited(), nil
			}}),
		WorktreeRoot: t.TempDir(),
	})
	t.Cleanup(h.Close)
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

func setHub(svc *Hub, h *hub.Hub) {
	ConnectHub(svc, h, nil)
}

func ctxb() context.Context { return context.Background() }

// M1：帶 LaunchSnapshot 的 Command → mygoipc.needs_profile，
// 完全沒碰到 hub。
func TestCommandRejectsLaunches(t *testing.T) {
	svc := &Hub{}
	h := testHub(t)
	setHub(svc, h)
	for _, kind := range []task.CommandKind{task.CmdCreateSession, task.CmdFanOut} {
		c := task.Command{Kind: kind}
		if kind == task.CmdCreateSession {
			c.CreateSession = &task.CreateSession{}
		} else {
			c.FanOut = &task.FanOut{}
		}
		var we *transport.Error
		if _, err := svc.Command(ctxb(), "t-x", c); err == nil ||
			!errors.As(err, &we) || we.Code != string(CodeNeedsProfile) {
			t.Fatalf("%s → %v", kind, err)
		}
	}
	if v, _ := h.Tasks(ctxb(), hub.LocalOwner); len(v) != 0 {
		t.Fatal("hub was called")
	}
}

// M2：有代碼的錯誤 → Wire JSON；沒代碼的 → transport.internal 且
// 原文記在 Error 級。
func TestWireErrors(t *testing.T) {
	svc := &Hub{}
	setHub(svc, testHub(t))
	_, err := svc.Command(ctxb(), "t-none", task.Command{
		Kind: task.CmdSetStatus})
	var we *transport.Error
	if err == nil || !errors.As(err, &we) || we.Code != "hub.unknown_task" {
		t.Fatalf("%v", err)
	}
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	e := wire(log, "Command", errors.New("disk on fire"))
	if e.Code != string(transport.CodeInternal) {
		t.Fatalf("%v", e)
	}
	if !strings.Contains(buf.String(), "disk on fire") ||
		!strings.Contains(buf.String(), "Command") {
		t.Fatalf("log %q", buf.String())
	}
}

// DL01、DL03：DeleteTask 以桌面身分刪除；沒歸檔的回 hub.not_archived，什麼都不刪。
func TestDeleteTask(t *testing.T) {
	svc := &Hub{}
	h := testHub(t)
	setHub(svc, h)
	id, _, err := h.CreateTask(ctxb(), hub.LocalOwner, t.TempDir(), task.CreateTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var we *transport.Error
	if err := svc.DeleteTask(ctxb(), id); !errors.As(err, &we) || we.Code != "hub.not_archived" {
		t.Fatalf("unarchived: %v", err)
	}
	if _, err := svc.Command(ctxb(), id, task.Command{Kind: task.CmdArchive}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteTask(ctxb(), id); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.Tasks(ctxb(), hub.LocalOwner); len(v) != 0 {
		t.Fatalf("tasks %+v", v)
	}
}

// M3：subscribe 的 send 出錯 → subscribe 返回；actor 仍可接受命令。
func TestSubscribeAdapterSendError(t *testing.T) {
	svc := &Hub{}
	h := testHub(t)
	setHub(svc, h)
	dir := t.TempDir()
	id, _, err := h.CreateTask(ctxb(), hub.LocalOwner, dir,
		task.CreateTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("page closed")
	if err := pageLeft(ctxb(), h.Subscribe(ctxb(), hub.LocalOwner, id, 0,
		func(hub.Update) error { return boom })); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	if _, err := h.Command(ctxb(), hub.LocalOwner, id, task.Command{
		Kind: task.CmdEditTask, EditTask: &task.EditTask{Title: "n"}}); err != nil {
		t.Fatalf("actor dead: %v", err)
	}
}

// M5：頁面離開（Send 回 ErrChannelClosed，或 ctx 已取消時 Send 出錯）時 Subscribe 與 WatchTasks
// 正常結束，不當成錯誤；其他錯誤照舊回傳（M3）。
func TestStreamsEndWhenPageLeaves(t *testing.T) {
	h := testHub(t)
	id, _, err := h.CreateTask(ctxb(), hub.LocalOwner, t.TempDir(), task.CreateTask{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctxb())
	cancel()
	pageGone := errors.New("page went away")
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		sendErr error
	}{
		{"channel closed", ctxb(), mygo.ErrChannelClosed},
		{"call canceled", cancelled, pageGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := pageLeft(tc.ctx, h.Subscribe(tc.ctx, hub.LocalOwner, id, 0, func(hub.Update) error { return tc.sendErr })); err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			if err := pageLeft(tc.ctx, h.WatchTasks(tc.ctx, hub.LocalOwner, func(hub.TaskView) error { return tc.sendErr })); err != nil {
				t.Fatalf("watchTasks: %v", err)
			}
		})
	}
}

// M4：產生的 TypeScript 含兩個 service、兩個事件、camelCase 欄位。
var bindOnce sync.Once // BindAs 是进程级的：-count>1 时只绑一次

func TestGenerateTypeScript(t *testing.T) {
	bindOnce.Do(func() {
		mygo.BindAs("Hub", &Hub{})
		mygo.BindAs("Agents", &Agents{})
		mygo.BindAs("Desktop", &Desktop{})
		mygo.BindAs("Usage", &Usage{})
		mygo.BindAs("Providers", &Providers{})
		mygo.BindAs("Keys", &Keys{})
		mygo.BindAs("Integrations", &Integrations{})
	})
	ts, err := mygo.GenerateTypeScript()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Hub", "Agents", "Desktop", "agentsChanged",
		"openTask", "Update", "Delta", "chooseFolder", "RegistryList",
		"NotifySettings", "watchUpdates", "checkForUpdates", "restartToUpdate",
		"fetchedAt", `version:`, `sessions:`, "Usage", "summary", "ReadState", "periods",
		"Providers", "ProviderRow", "wholePage", "Keys", "StoredKey", "addedAt", "baseUrl", "KeyIcon",
		"Integrations", `export type IntegrationStatus = "unknown" | "unavailable" | "available" | "connected";`} {
		if !strings.Contains(string(ts), want) {
			t.Fatalf("missing %q in generated client", want)
		}
	}
	// Y1：模組路徑的第一段必須有點，否則 mygo 把我們的套件當
	// 標準庫、不讀原始碼，產生的型別就只剩 string。
	for _, want := range []string{
		`export type Availability = "missing" | "broken" | "outdated" | "ready";`,
		`export type TaskStatus = "todo" | "in_progress" | "in_review" | "done";`,
		// 更新狀態的型別名不能被別的套件的同名型別擠成帶前綴的名字。
		`export type Phase = "unavailable" | "idle" | "checking" | "downloading" | "installing" | "ready";`,
		`export interface Status {`,
		// 用量的型別名：不能被擠成帶前綴的名字，也不能遮住 DOM 的型別（Range、Window）。
		`export type Category = "input" | "cacheRead" | "cacheWrite" | "output";`,
		`export type Note = "not_recorded" | "session_end" | "turn_end" | "reported_only";`,
		`export interface Summary {`,
		`export interface Period {`,
		`export interface Limit {`,
		// 廠商狀態的型別名不能被擠成帶前綴的名字。
		`export type Health = "operational" | "degraded" | "outage";`,
		`export interface Board {`,
		`export interface ProviderDay {`,
		`export interface StoredKey {`,
		`export interface KeyIcon {`,
	} {
		if !strings.Contains(string(ts), want) {
			t.Fatalf("missing %q in generated client", want)
		}
	}
	for _, banned := range []string{"setHub", "setSource", "SetHub", "SetSource"} {
		if strings.Contains(string(ts), banned) {
			t.Fatalf("backend wiring leaked into page API: %q", banned)
		}
	}
}

// 未 Connect 的呼叫 → mygoipc.not_ready。
func TestNotReady(t *testing.T) {
	svc := &Hub{}
	var we *transport.Error
	if _, err := svc.Tasks(ctxb()); err == nil ||
		!errors.As(err, &we) || we.Code != string(CodeNotReady) {
		t.Fatalf("%v", err)
	}
	ag := &Agents{}
	var we2 *transport.Error
	if _, err := ag.List(ctxb()); err == nil ||
		!errors.As(err, &we2) || we2.Code != string(CodeNotReady) {
		t.Fatalf("%v", err)
	}
	// Desktop 接上之前不能改語言。
	var we3 *transport.Error
	if err := (&Desktop{}).SetLanguage("en"); err == nil ||
		!errors.As(err, &we3) || we3.Code != string(CodeNotReady) {
		t.Fatalf("%v", err)
	}
	// Desktop 接上之前不能讀寫防止睡眠的開關：不猜預設值。
	var weKA, weKA2 *transport.Error
	if err := (&Desktop{}).SetKeepAwake(false); err == nil ||
		!errors.As(err, &weKA) || weKA.Code != string(CodeNotReady) {
		t.Fatalf("%v", err)
	}
	if _, err := (&Desktop{}).KeepAwake(); err == nil ||
		!errors.As(err, &weKA2) || weKA2.Code != string(CodeNotReady) {
		t.Fatalf("%v", err)
	}
	// Desktop 接上之前不能讀寫外觀：不猜（PF02）。
	var weTh, weTh2 *transport.Error
	if _, err := (&Desktop{}).Theme(); err == nil || !errors.As(err, &weTh) || weTh.Code != string(CodeNotReady) {
		t.Fatalf("Theme: %v", err)
	}
	if err := (&Desktop{}).SetTheme("dark"); err == nil || !errors.As(err, &weTh2) || weTh2.Code != string(CodeNotReady) {
		t.Fatalf("SetTheme: %v", err)
	}
	// 沒接上自動更新時，更新的方法回 not_ready（重啟什麼都不做）。
	var we4, we5 *transport.Error
	if _, err := (&Desktop{}).CheckForUpdates(ctxb()); err == nil ||
		!errors.As(err, &we4) || we4.Code != string(CodeNotReady) {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if err := (&Desktop{}).WatchUpdates(ctxb(), nil); err == nil ||
		!errors.As(err, &we5) || we5.Code != string(CodeNotReady) {
		t.Fatalf("WatchUpdates: %v", err)
	}
	(&Desktop{}).RestartToUpdate()
	// 沒接上用量時三個方法都回 not_ready。
	for name, call := range map[string]func() error{
		"Summary": func() error { _, err := (&Usage{}).Summary(ctxb()); return err },
		"Refresh": func() error { _, err := (&Usage{}).Refresh(ctxb()); return err },
		"Watch":   func() error { return (&Usage{}).Watch(ctxb(), nil) },
	} {
		var we *transport.Error
		if err := call(); err == nil || !errors.As(err, &we) || we.Code != string(CodeNotReady) {
			t.Fatalf("Usage.%s: %v", name, err)
		}
	}
	for name, call := range map[string]func() error{
		"Refresh":                 func() error { _, err := (&Providers{}).Refresh(ctxb()); return err },
		"Watch":                   func() error { return (&Providers{}).Watch(ctxb(), nil) },
		"List":                    func() error { _, err := (&Keys{}).List(ctxb()); return err },
		"Icons":                   func() error { _, err := (&Keys{}).Icons(); return err },
		"Add":                     func() error { _, err := (&Keys{}).Add(ctxb(), "Work", "anthropic", "k", ""); return err },
		"SetBaseURL":              func() error { _, err := (&Keys{}).SetBaseURL(ctxb(), "id", ""); return err },
		"Remove":                  func() error { _, err := (&Keys{}).Remove(ctxb(), "id"); return err },
		"Integrations.Refresh":    func() error { _, err := (&Integrations{}).Refresh(ctxb()); return err },
		"Integrations.Connect":    func() error { _, err := (&Integrations{}).Connect(ctxb(), integration.GitHub); return err },
		"Integrations.Disconnect": func() error { _, err := (&Integrations{}).Disconnect(ctxb(), integration.GitHub); return err },
		"Integrations.Watch":      func() error { return (&Integrations{}).Watch(ctxb(), nil) },
	} {
		var we *transport.Error
		if err := call(); err == nil || !errors.As(err, &we) || we.Code != string(CodeNotReady) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// DV18：沒有遠程的構建（正式版）停用 Remote，每個方法立即回 mygoipc.remote_unavailable；
// 沒停用、只是還沒接上時仍是 mygoipc.not_ready（頁面稍後再試）。
func TestRemoteDisabled(t *testing.T) {
	calls := func(s *Remote) map[string]func() error {
		return map[string]func() error{
			"Status":       func() error { _, err := s.Status(); return err },
			"Watch":        func() error { return s.Watch(ctxb(), nil) },
			"SetOn":        func() error { return s.SetOn(true) },
			"SetRelay":     func() error { return s.SetRelay("wss://relay.example") },
			"Retry":        func() error { return s.Retry() },
			"Offer":        func() error { _, err := s.Offer(); return err },
			"CancelOffer":  func() error { return s.CancelOffer() },
			"Reset":        func() error { return s.Reset(ctxb()) },
			"WatchDevices": func() error { return s.WatchDevices(ctxb(), nil) },
			"Rename":       func() error { _, err := s.Rename(ctxb(), "d", "Phone"); return err },
			"Revoke":       func() error { return s.Revoke(ctxb(), "d") },
		}
	}
	for _, tc := range []struct {
		name    string
		disable bool
		want    Code
	}{
		{name: "not connected yet", want: CodeNotReady},
		{name: "build without remote", disable: true, want: CodeRemoteUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Remote{}
			if tc.disable {
				DisableRemote(s)
			}
			for name, call := range calls(s) {
				var we *transport.Error
				if err := call(); !errors.As(err, &we) || we.Code != string(tc.want) {
					t.Errorf("Remote.%s: %v, want %s", name, err, tc.want)
				}
			}
		})
	}
}

// C1：綁定型別的導出方法就是頁面 API（D61），方法集用白名單鎖住。
func TestBoundMethodsArePageAPI(t *testing.T) {
	want := map[reflect.Type][]string{
		reflect.TypeFor[*Hub](): {"Attention", "Command", "Confirmations",
			"CreateTask", "DeleteLabel", "DeleteRule", "DeleteTask", "Files", "Labels", "Models", "PutLabel", "PutSettings", "PutWorkspace", "Recap",
			"Rules", "Search", "SetDefaultModel", "Settings", "Snapshot", "StartAgent", "Subscribe",
			"Tasks", "WatchTasks", "Workspaces"},
		reflect.TypeFor[*Agents](): {"List", "Refresh", "Registry"},
		reflect.TypeFor[*Desktop](): {"Attachable", "CheckForUpdates", "ChooseFiles", "ChooseFolder", "ContinueWithoutAccount", "Folder", "KeepAwake", "Languages",
			"OpenLink", "Privacy", "Quit", "RecentSearches", "ReleaseNotesShown", "RemoteAvailable", "RestartToUpdate", "RevealFolder", "SetKeepAwake", "SetLanguage", "SetRecentSearches", "SetTheme", "ShowMain", "ShowTask", "Theme",
			"WatchReleaseNotes", "WatchUpdates", "WithoutAccount"},
		reflect.TypeFor[*Usage]():        {"Refresh", "Summary", "Watch"},
		reflect.TypeFor[*Providers]():    {"Refresh", "Watch"},
		reflect.TypeFor[*Keys]():         {"Add", "Icons", "List", "Remove", "SetBaseURL"},
		reflect.TypeFor[*Integrations](): {"Connect", "Disconnect", "Refresh", "Watch"},
		reflect.TypeFor[*Remote](): {"CancelOffer", "Offer", "Rename", "Reset", "Retry", "Revoke", "SetOn", "SetRelay",
			"Status", "Watch", "WatchDevices"},
	}
	for typ, names := range want {
		var got []string
		for i := range typ.NumMethod() {
			m := typ.Method(i)
			if m.IsExported() {
				got = append(got, m.Name)
			}
		}
		slices.Sort(got)
		slices.Sort(names)
		if !slices.Equal(got, names) {
			t.Fatalf("%v exported methods = %v, want %v", typ, got, names)
		}
	}
}

// IG06、IG09：集成的錯誤經 Wire 帶代碼與參數到頁面。gh 不在 PATH 裡（空目錄），連接回 gh_missing；
// 不認得的 ID 回 unknown{id}。
func TestIntegrationsWire(t *testing.T) {
	runner := &proc.Runner{}
	t.Cleanup(runner.Close)
	dir := t.TempDir()
	svc, err := integration.New(ctxb(), integration.Options{Runner: runner, Store: &integration.MemStore{}, Root: dir,
		Env: func(context.Context) (proc.Env, error) { return proc.Env{"PATH=" + dir, "HOME=" + dir}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	s := &Integrations{}
	ConnectIntegrations(s, svc, nil)
	var we *transport.Error
	if _, err := s.Connect(ctxb(), integration.GitHub); !errors.As(err, &we) || we.Code != string(integration.CodeGhMissing) {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := s.Disconnect(ctxb(), "gitlab"); !errors.As(err, &we) || we.Code != string(integration.CodeUnknown) || we.Params["id"] != "gitlab" {
		t.Fatalf("Disconnect: %v", err)
	}
	cat, err := s.Refresh(ctxb())
	if err != nil || len(cat.Items) != 1 || cat.Items[0].Status != integration.StatusUnavailable {
		t.Fatalf("Refresh: %+v %v", cat, err)
	}
}
