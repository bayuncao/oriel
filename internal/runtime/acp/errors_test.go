package acp

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

// TestCodes 確認 errors.go 宣告的每個代碼都有說明、以 "acp." 開頭且不重複，
// Codes 也列出了全部。
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
		if !strings.HasPrefix(string(code), "acp.") || codeText[code] == "" {
			t.Errorf("%s = %q needs the acp. prefix and a text", name, code)
		}
	}
	if got := CodeRequestFailed.Params(); !slices.Equal(got, []string{"method", "detail"}) {
		t.Errorf("params %v", got)
	}
	err = fmt.Errorf("start: %w", fail(CodeAuthRequired, nil, "agent", "Grok"))
	if !errors.Is(err, &Error{Code: CodeAuthRequired}) || errors.Is(err, &Error{Code: CodeRefusal}) {
		t.Errorf("matching by code: %v", err)
	}
}

// 純函數：截斷、命令、用量換算（C16、C27、D100）。
func TestMapping(t *testing.T) {
	long := strings.Repeat("é", maxToolOutput) // 每個字 2 個位元組
	if got := truncate(long); len(got) > maxToolOutput || !strings.HasSuffix(got, "\n…") || !strings.HasPrefix(got, "éé") {
		t.Errorf("truncate: %d bytes, suffix %q", len(got), got[len(got)-8:])
	}
	if got := truncate("short"); got != "short" {
		t.Errorf("truncate short: %q", got)
	}
	for _, tc := range []struct {
		input any
		want  string
	}{
		{map[string]any{"command": "ls -la"}, "ls -la"},
		{map[string]any{"command": []any{"git", "commit", "-m", "it's done"}}, `git commit -m 'it'\''s done'`},
		{map[string]any{"command": []any{"echo", ""}}, "echo ''"},
		{map[string]any{"command": []any{"x", 1.0}}, ""},
		{map[string]any{"cmd": "ls"}, ""},
		{"ls", ""},
	} {
		if got := command("execute", tc.input); got != tc.want {
			t.Errorf("command(%v) = %q, want %q", tc.input, got, tc.want)
		}
	}
	if got := command("read", map[string]any{"command": "ls"}); got != "" {
		t.Errorf("command for read = %q", got)
	}
}
