package task

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// TestReasons 確認 errors.go 宣告的每個 Reason 都在 Reasons 裡，且不重複。
func TestReasons(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "errors.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []Reason
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Reason" {
			return true
		}
		for i, v := range vs.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s is not a string literal", vs.Names[i].Name)
				continue
			}
			s, _ := strconv.Unquote(lit.Value)
			declared = append(declared, Reason(s))
		}
		return true
	})
	listed := Reasons()
	slices.Sort(declared)
	slices.Sort(listed)
	if !slices.Equal(declared, listed) || len(slices.Compact(slices.Clone(listed))) != len(listed) {
		t.Fatalf("declared %v, listed %v", declared, listed)
	}
}

// TestProblemJSON 確認 Problem 的形狀與前端的 Problem（src/errors.ts）一致。
func TestProblemJSON(t *testing.T) {
	for _, tc := range []struct {
		p    Problem
		want string
	}{
		{Problem{Code: "proc.signaled", Params: map[string]string{"signal": "9"}}, `{"code":"proc.signaled","params":{"signal":"9"}}`},
		{Problem{Code: "proc.timed_out"}, `{"code":"proc.timed_out"}`},
	} {
		if b, err := json.Marshal(tc.p); err != nil || string(b) != tc.want {
			t.Errorf("Marshal = %s, %v; want %s", b, err, tc.want)
		}
	}
}
