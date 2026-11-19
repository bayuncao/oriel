package store

import (
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

// TestCodes 解析 errors.go：每個 Code 常數都在 codeText 有說明、
// 有 store. 前綴、不重複。
func TestCodes(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "errors.go", nil, 0)
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
		if !strings.HasPrefix(string(code), "store.") {
			t.Errorf("%s = %q lacks the store. prefix", name, code)
		}
		if codeText[code] == "" {
			t.Errorf("%s = %q has no text", name, code)
		}
	}
	if !slices.IsSorted(Codes()) {
		t.Error("Codes not sorted")
	}
}

// TestParams 代碼的參數來自文字裡的 {placeholder}。
func TestParams(t *testing.T) {
	if got := CodeNewerSchema.Params(); !slices.Equal(got, []string{"version"}) {
		t.Fatalf("params %v", got)
	}
	if got := CodeClosed.Params(); len(got) != 0 {
		t.Fatalf("params %v", got)
	}
}

// TestIs 依代碼比較；參數不同的同代碼錯誤仍相等。
func TestIs(t *testing.T) {
	a := fail(CodeConflict, nil, "task", "t-1")
	b := fail(CodeConflict, nil, "task", "t-2")
	if !errors.Is(a, b) {
		t.Fatal("same code should match")
	}
	if errors.Is(a, fail(CodeClosed, nil)) {
		t.Fatal("different code matched")
	}
	if !strings.Contains(a.Error(), "t-1") {
		t.Fatalf("Error() %q", a.Error())
	}
}
