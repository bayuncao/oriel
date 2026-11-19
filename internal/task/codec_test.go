package task

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// eventKinds 解析 event.go，回傳宣告的每個 EventKind 常數的名稱。
func eventKinds(t *testing.T) map[EventKind]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "event.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[EventKind]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "EventKind" {
			return true
		}
		for i, v := range vs.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("%s is not a string literal", vs.Names[i].Name)
			}
			kinds[EventKind(strings.Trim(lit.Value, `"`))] = vs.Names[i].Name
		}
		return true
	})
	return kinds
}

// TestCodecCoversEvent：每個 Kind 都在 payloads 裡，每個 payload 欄位
// （Event 的指針欄位）都恰好有一個 slot，名稱就是它的 JSON 鍵。
func TestCodecCoversEvent(t *testing.T) {
	for k, name := range eventKinds(t) {
		if _, ok := payloads[k]; !ok {
			t.Errorf("%s has no entry in payloads", name)
		}
	}
	if got, want := len(payloads), len(eventKinds(t)); got != want {
		t.Errorf("payloads has %d kinds, event.go declares %d", got, want)
	}
	var fields, names []string
	typ := reflect.TypeFor[Event]()
	for i := range typ.NumField() {
		if f := typ.Field(i); f.Type.Kind() == reflect.Pointer {
			key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			fields = append(fields, key)
		}
	}
	for _, s := range slots {
		names = append(names, s.name)
	}
	slices.Sort(fields)
	slices.Sort(names)
	if !slices.Equal(fields, names) {
		t.Errorf("payload fields %v, slots %v", fields, names)
	}
}

// TestSamplesCoverKinds 確保下面以 samples 為基礎的測試涵蓋每個 Kind。
func TestSamplesCoverKinds(t *testing.T) {
	seen := map[EventKind]bool{}
	for _, e := range samples() {
		seen[e.Kind] = true
	}
	for k, name := range eventKinds(t) {
		if !seen[k] {
			t.Errorf("no sample for %s", name)
		}
	}
}

func TestEventRoundTrip(t *testing.T) {
	for _, e := range samples() {
		t.Run(string(e.Kind), func(t *testing.T) {
			b, err := EncodeEvent(e)
			if err != nil {
				t.Fatal(err)
			}
			var env map[string]json.RawMessage
			if err := json.Unmarshal(b, &env); err != nil || string(env["v"]) != "1" {
				t.Fatalf("envelope %s", b)
			}
			for _, column := range []string{"seq", "kind", "at"} {
				if _, ok := env[column]; ok {
					t.Errorf("envelope carries the column %s: %s", column, b)
				}
			}
			got, err := DecodeEvent(e.Seq, e.Kind, e.At, b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, e) {
				t.Fatalf("decoded\n%+v\nwant\n%+v", got, e)
			}
		})
	}
}

func TestEncodeRejectsBadPayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    Event
	}{
		{"unknown kind", Event{Kind: "Nope"}},
		{"missing payload", Event{Kind: TaskCreatedKind}},
		{"payload on a kind without one", Event{Kind: TaskArchivedKind, TaskEdited: &EditTask{}}},
		{"two payloads", Event{Kind: TaskEditedKind, TaskEdited: &EditTask{}, PlanUpdated: &PlanUpdate{}}},
		{"another kind's payload", Event{Kind: ForkMergedKind, Discarded: &IntentRef{}}},
	} {
		if _, err := EncodeEvent(tc.e); err == nil {
			t.Errorf("%s: encoded", tc.name)
		}
	}
}

func TestDecodeRejectsBadRows(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    EventKind
		payload string
	}{
		{"unknown kind", "Nope", `{"v":1}`},
		{"not json", TaskArchivedKind, `{`},
		{"newer version", TaskArchivedKind, `{"v":2}`},
		{"no version", TaskArchivedKind, `{}`},
		{"missing data", TaskEditedKind, `{"v":1}`},
		{"null data", TaskEditedKind, `{"v":1,"data":null}`},
		{"data on a kind without payload", TaskArchivedKind, `{"v":1,"data":{}}`},
		{"wrong data shape", TaskEditedKind, `{"v":1,"data":"x"}`},
	} {
		if _, err := DecodeEvent(1, tc.kind, at0, []byte(tc.payload)); err == nil {
			t.Errorf("%s: decoded", tc.name)
		}
	}
}

// TestUpcast 以臨時登記的版本 2 確認舊 payload 會經 upcaster 轉換，
// 缺少 upcaster 時報錯。
func TestUpcast(t *testing.T) {
	versions[TaskEditedKind] = 2
	t.Cleanup(func() { delete(versions, TaskEditedKind) })
	old := []byte(`{"v":1,"data":{"name":"Old","brief":"b"}}`)
	if _, err := DecodeEvent(1, TaskEditedKind, at0, old); err == nil {
		t.Fatal("decoded without an upcaster")
	}
	upcasters[upcastKey{TaskEditedKind, 1}] = func(d json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(strings.Replace(string(d), `"name"`, `"title"`, 1)), nil
	}
	t.Cleanup(func() { delete(upcasters, upcastKey{TaskEditedKind, 1}) })
	e, err := DecodeEvent(1, TaskEditedKind, at0, old)
	if err != nil || e.TaskEdited.Title != "Old" {
		t.Fatalf("DecodeEvent = %+v, %v", e.TaskEdited, err)
	}
	if b, _ := EncodeEvent(e); !strings.Contains(string(b), `"v":2`) {
		t.Fatalf("re-encoded as %s", b)
	}
}
