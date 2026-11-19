package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/task"
)

// TestCodes 確認 errors.go 宣告的每個 Code 都在 Codes 裡、以
// "transport." 開頭且不重複。
func TestCodes(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "errors.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []Code
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Code" {
			return true
		}
		for i, v := range vs.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s is not a string literal", vs.Names[i].Name)
				continue
			}
			s, _ := strconv.Unquote(lit.Value)
			if !strings.HasPrefix(s, "transport.") {
				t.Errorf("%s = %q lacks the transport. prefix", vs.Names[i].Name, s)
			}
			declared = append(declared, Code(s))
		}
		return true
	})
	listed := Codes()
	slices.Sort(declared)
	slices.Sort(listed)
	if !slices.Equal(declared, listed) || len(slices.Compact(slices.Clone(listed))) != len(listed) {
		t.Fatalf("declared %v, listed %v", declared, listed)
	}
}

func TestWire(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		code  string
		known bool
	}{
		{"agent", fmt.Errorf("verify: %w", agent.ErrUnknownAgent), "agent.unknown_agent", true},
		{"task", &task.Rejection{Reason: task.ReasonArchived, Detail: "since Monday"}, "task.archived", true},
		{"canceled", fmt.Errorf("wait: %w", context.Canceled), string(CodeCanceled), true},
		{"deadline", context.DeadlineExceeded, string(CodeTimedOut), true},
		{"unknown", errors.New("disk on fire"), string(CodeInternal), false},
		{"empty code", &Error{}, string(CodeInternal), false},
		{"nil", nil, string(CodeInternal), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := Wire(tc.err)
			if got == nil || got.Code != tc.code || len(got.Params) != 0 || known != tc.known {
				t.Fatalf("Wire = %+v, %v", got, known)
			}
		})
	}
}

// TestWireKeepsParams 也確認沒有原文外洩：前端只看到代碼與參數。
func TestWireKeepsParams(t *testing.T) {
	_, err := (agent.CustomAgent{Slug: "Bad Slug", Command: "x"}).Definition()
	e, known := Wire(err)
	if !known || e.Code != "agent.invalid_slug" || e.Params["slug"] != "Bad Slug" {
		t.Fatalf("Wire = %+v, %v", e, known)
	}
	var back Error
	if err := json.Unmarshal([]byte(e.Error()), &back); err != nil || back.Code != e.Code || back.Params["slug"] != "Bad Slug" {
		t.Fatalf("Error() = %s", e.Error())
	}
	if unknown, _ := Wire(errors.New("secret path /Users/me")); unknown.Error() != `{"code":"transport.internal"}` {
		t.Fatalf("unknown error leaked: %s", unknown.Error())
	}
}

// TestWireIsIdempotent：已轉換的錯誤再被包一層、再經過 Wire，代碼與參數不變。
func TestWireIsIdempotent(t *testing.T) {
	_, err := (agent.CustomAgent{Slug: "Bad Slug", Command: "x"}).Definition()
	first, _ := Wire(err)
	again, known := Wire(fmt.Errorf("bind: %w", first))
	if !known || again.Code != first.Code || again.Params["slug"] != "Bad Slug" {
		t.Fatalf("Wire(Wire) = %+v, %v", again, known)
	}
}

// TestWireCopiesParams：修改結果不影響源錯誤，哨兵錯誤是全域值。
func TestWireCopiesParams(t *testing.T) {
	src := &Error{Code: "agent.not_runnable", Params: map[string]string{"platform": "linux-arm64"}}
	e, _ := Wire(src)
	e.Params["platform"] = "changed"
	if src.Params["platform"] != "linux-arm64" {
		t.Fatalf("source params changed: %v", src.Params)
	}
}
