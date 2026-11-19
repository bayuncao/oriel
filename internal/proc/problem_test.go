package proc

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestCodes 確認 problem.go 宣告的每個代碼都有說明、以 "proc." 開頭
// 且不重複，Codes 也列出了全部。
func TestCodes(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "problem.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[Code]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || vs.Type == nil || fmt.Sprint(vs.Type) != "Code" {
			return true
		}
		for i, name := range vs.Names {
			v, _ := strconv.Unquote(vs.Values[i].(*ast.BasicLit).Value)
			if prev, dup := declared[Code(v)]; dup {
				t.Errorf("%s and %s share %q", prev, name.Name, v)
			}
			declared[Code(v)] = name.Name
		}
		return true
	})
	if len(declared) != len(Codes()) {
		t.Errorf("%d codes declared, %d listed", len(declared), len(Codes()))
	}
	for code, name := range declared {
		if !strings.HasPrefix(string(code), "proc.") {
			t.Errorf("%s = %q lacks the proc. prefix", name, code)
		}
		if codeText[code] == "" {
			t.Errorf("%s has no text", name)
		}
	}
	if got := CodeStartFailed.Params(); !slices.Equal(got, []string{"path", "detail"}) {
		t.Errorf("params %v", got)
	}
}

func TestErrorMatching(t *testing.T) {
	err := fmt.Errorf("probe: %w", fail(CodeTimedOut, context.DeadlineExceeded))
	if !errors.Is(err, &Error{Code: CodeTimedOut}) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("code or cause lost: %v", err)
	}
	if errors.Is(err, ErrClosed) {
		t.Errorf("matched another code: %v", err)
	}
	if got := fail(CodeExited, nil, "status", "3").Error(); got != "proc: exited with status 3" {
		t.Errorf("Error() = %q", got)
	}
}
