package provider

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestCodes 解析 problem.go：每個 Code 常數都在 codeText 有說明、
// 有 provider. 前綴、不與其他代碼重複。
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
		if !strings.HasPrefix(string(code), "provider.") {
			t.Errorf("%s = %q lacks the provider. prefix", name, code)
		}
		if codeText[code] == "" {
			t.Errorf("%s has no text", name)
		}
	}
}
