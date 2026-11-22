package acp

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// 模型設置（m2.md C45–C47，D118）。

// configOptions 是一份會話設置：一個布林、一個 mode 分類的 select、一個 model 分類、分組的 select（目前 cur）。
func configOptions(cur string) string {
	return `[{"type":"boolean","id":"web","name":"Web","currentValue":true},` +
		`{"type":"select","id":"mode","name":"Mode","category":"mode","currentValue":"build","options":[{"value":"build","name":"Build"}]},` +
		`{"type":"select","id":"model","name":"Model","category":"model","currentValue":"` + cur + `","options":[` +
		`{"group":"fast","name":"Fast","options":[{"value":"mini","name":"Mini","description":"Quick"}]},` +
		`{"group":"deep","name":"Deep","options":[{"value":"max","name":"Max"}]}]}]`
}

func wantModel(cur string) *task.ModelSetting {
	return &task.ModelSetting{Option: "model", Current: cur, Choices: []task.ModelChoice{
		{Value: "mini", Name: "Mini", Description: "Quick", Group: "Fast"},
		{Value: "max", Name: "Max", Group: "Deep"},
	}}
}

// C45：session/new 的回應帶設置時，先報 model 分類那一項（分組攤平、帶組名），再綁定。
func TestModelReportedAtStart(t *testing.T) {
	r := newRuntime(t, Options{})
	o, err := start(t, r, testLaunch(), "", fixture(t, lInit, lInitOK, lNew,
		lResult(2, `{"sessionId":"s1","configOptions":`+configOptions("mini")+`}`)))
	if err != nil {
		t.Fatal(err)
	}
	if m := startFacts(t, o.rec, "s1"); !reflect.DeepEqual(m, wantModel("mini")) {
		t.Fatalf("model %+v, want %+v", m, wantModel("mini"))
	}
}

// C45：接回（session/resume）時同樣報；只有 mode、沒有 model 分類時報 nil。
func TestModelReportedAtResume(t *testing.T) {
	r := newRuntime(t, Options{})
	init := `{"agent":{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":{}}}}}}`
	resume := `{"client":{"id":2,"method":"session/resume","params":{"cwd":"<cwd>","sessionId":"s1"}}}`
	o, err := start(t, r, testLaunch(), "s1", fixture(t, lInit, init, resume,
		lResult(2, `{"configOptions":[{"type":"select","id":"mode","name":"Mode","category":"mode","currentValue":"build","options":[{"value":"build","name":"Build"}]}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if m := startFacts(t, o.rec, "s1"); m != nil {
		t.Fatalf("model %+v, want nil without a model option", m)
	}
}

// C49：握手中（回應之前）的 config_option_update 不另報（不然是“agent 自己換了”）；回應沒帶設置時，Start 的報告用最後一則。
func TestModelUpdatedDuringStart(t *testing.T) {
	r := newRuntime(t, Options{})
	o, err := start(t, r, testLaunch(), "", fixture(t, lInit, lInitOK, lNew,
		lUpdate(`{"sessionUpdate":"config_option_update","configOptions":`+configOptions("mini")+`}`),
		lUpdate(`{"sessionUpdate":"config_option_update","configOptions":`+configOptions("max")+`}`),
		lNewOK))
	if err != nil {
		t.Fatal(err)
	}
	if m := startFacts(t, o.rec, "s1"); !reflect.DeepEqual(m, wantModel("max")) {
		t.Fatalf("model %+v, want the last update %+v", m, wantModel("max"))
	}
	if k := o.rec.kinds(); len(k) != 2 {
		t.Fatalf("facts %v: updates during the handshake are not reported by themselves", k)
	}
}

// C49：回應帶了設置時以回應為準（比它之前的更新新）。
func TestModelResponseWinsOverStartUpdate(t *testing.T) {
	r := newRuntime(t, Options{})
	o, err := start(t, r, testLaunch(), "", fixture(t, lInit, lInitOK, lNew,
		lUpdate(`{"sessionUpdate":"config_option_update","configOptions":`+configOptions("max")+`}`),
		lResult(2, `{"sessionId":"s1","configOptions":`+configOptions("mini")+`}`)))
	if err != nil {
		t.Fatal(err)
	}
	if m := startFacts(t, o.rec, "s1"); !reflect.DeepEqual(m, wantModel("mini")) {
		t.Fatalf("model %+v, want the response's %+v", m, wantModel("mini"))
	}
}

// C46：之後的 config_option_update 報 model_reported（不是 start）。
func TestModelUpdated(t *testing.T) {
	r := newRuntime(t, Options{})
	o := mustStart(t, r, testLaunch(), "", fixture(t, append(append([]string{}, started...),
		lUpdate(`{"sessionUpdate":"config_option_update","configOptions":`+configOptions("max")+`}`))...))
	c := o.rec.next(t, is(task.FactModelReported))
	if c.ModelReported.Start || !reflect.DeepEqual(c.ModelReported.Setting, wantModel("max")) {
		t.Fatalf("update %+v", c.ModelReported)
	}
}

// C47：SetModel 送 session/set_config_option，回傳回應裡的模型設置，不另報事實；agent 回錯誤時是 acp.request_failed。
func TestSetModel(t *testing.T) {
	r := newRuntime(t, Options{})
	set := func(id int) string {
		return `{"client":{"id":` + string(rune('0'+id)) + `,"method":"session/set_config_option","params":{"configId":"model","sessionId":"s1","value":"max"}}}`
	}
	o := mustStart(t, r, testLaunch(), "", fixture(t, append(append([]string{}, started...),
		set(3), lResult(3, `{"configOptions":`+configOptions("max")+`}`),
		set(4), `{"agent":{"jsonrpc":"2.0","id":4,"error":{"code":-32602,"message":"unknown model"}}}`)...))
	m, err := o.conn.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "max"})
	if err != nil || !reflect.DeepEqual(m, wantModel("max")) {
		t.Fatalf("SetModel = %+v, %v", m, err)
	}
	_, err = o.conn.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "max"})
	if code := codeOf(err); code != string(CodeRequestFailed) {
		t.Fatalf("err %v (%s), want %s", err, code, CodeRequestFailed)
	}
	if k := o.rec.kinds(); len(k) != 2 {
		t.Fatalf("facts %v: SetModel reports nothing by itself", k)
	}
}

// C48：agent 在時限內沒回應換模型時是 acp.set_model_timed_out，連線照舊。
func TestSetModelTimedOut(t *testing.T) {
	r := newRuntime(t, Options{SetModelTimeout: time.Second})
	o := mustStart(t, r, testLaunch(), "", fixture(t, append(append([]string{}, started...),
		`{"client":{"id":3,"method":"session/set_config_option","params":{"configId":"model","sessionId":"s1","value":"max"}}}`)...))
	_, err := o.conn.SetModel(context.Background(), task.ModelSwitch{Option: "model", Value: "max"})
	if code := codeOf(err); code != string(CodeSetModelTimedOut) {
		t.Fatalf("err %v (%s), want %s", err, code, CodeSetModelTimedOut)
	}
	if k := o.rec.kinds(); len(k) != 2 {
		t.Fatalf("facts %v: the connection stays up", k)
	}
}

func codeOf(err error) string {
	if c, ok := err.(interface{ ErrorCode() string }); ok {
		return c.ErrorCode()
	}
	return ""
}

// C46、C49：route 按讀到的順序處理。握手回應之前的更新併進 Start 的報告；回應之後、綁定之前讀到的更新是 agent
// 啟動之後的變化，綁定之後另報（不能併進去丟掉：CI 上 handshake 拿到回應前讀取迴圈就讀到了它）。直接按順序餵
// route、再綁定，不靠排程重現那個先後。
func TestModelUpdatedBeforeBind(t *testing.T) {
	c := &conn{rpc: newRPCTrack(), out: newOutbox(), logger: slog.New(slog.DiscardHandler)}
	update := func(cur string) []byte {
		return []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":` +
			`{"sessionUpdate":"config_option_update","configOptions":` + configOptions(cur) + `}}}`)
	}
	if _, err := (tap{w: io.Discard, t: c.rpc}).Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	c.route(update("mini"), io.Discard) // 握手中
	c.route([]byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1"}}`), io.Discard)
	c.route(update("max"), io.Discard) // 回應之後、綁定之前
	c.bind("s1", nil)
	c.out.mu.Lock()
	items := slices.Clone(c.out.items)
	c.out.mu.Unlock()
	if len(items) != 3 || items[0].Kind != task.FactModelReported || !items[0].ModelReported.Start ||
		!reflect.DeepEqual(items[0].ModelReported.Setting, wantModel("mini")) || items[1].Kind != task.FactNativeBound ||
		items[2].Kind != task.FactModelReported || items[2].ModelReported.Start ||
		!reflect.DeepEqual(items[2].ModelReported.Setting, wantModel("max")) {
		t.Fatalf("facts %+v", items)
	}
}
