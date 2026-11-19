package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestProblemCodes 確認 problem.go 宣告的每個代碼都有說明、以
// "agent." 開頭且不重複，ProblemCodes 也列出了全部。
func TestProblemCodes(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "problem.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[ProblemCode]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || vs.Type == nil || fmt.Sprint(vs.Type) != "ProblemCode" {
			return true
		}
		for i, name := range vs.Names {
			v, _ := strconv.Unquote(vs.Values[i].(*ast.BasicLit).Value)
			if prev, dup := declared[ProblemCode(v)]; dup {
				t.Errorf("%s and %s share %q", prev, name.Name, v)
			}
			declared[ProblemCode(v)] = name.Name
		}
		return true
	})
	if len(declared) != len(ProblemCodes()) {
		t.Errorf("%d codes declared, %d listed", len(declared), len(ProblemCodes()))
	}
	for code, name := range declared {
		if !strings.HasPrefix(string(code), "agent.") {
			t.Errorf("%s = %q lacks the agent. prefix", name, code)
		}
		if problemText[code] == "" {
			t.Errorf("%s has no text", name)
		}
	}
}

// coded 是邊界（transport）辨認錯誤所用的介面。
type coded interface {
	error
	ErrorCode() string
	ErrorParams() map[string]string
}

func TestProblemAsError(t *testing.T) {
	cause := errors.New("disk full")
	err := fmt.Errorf("saving: %w", fail(CodeRegistryCacheWrite, cause, "detail", cause.Error()))
	if !errors.Is(err, ErrCacheWrite) || !errors.Is(err, cause) {
		t.Fatalf("errors.Is: %v", err)
	}
	if errors.Is(err, ErrClosed) {
		t.Fatal("matches a different code")
	}
	c, ok := errors.AsType[coded](err)
	if !ok || c.ErrorCode() != "agent.registry_cache_write" || c.ErrorParams()["detail"] != "disk full" {
		t.Fatalf("coded: %v, %v", c, ok)
	}
	if got := err.Error(); got != "saving: agent: cannot write the registry cache: disk full: disk full" {
		t.Errorf("Error() = %q", got)
	}
}

func TestProblemJSON(t *testing.T) {
	b, err := json.Marshal(Installation{Agent: "foo", Problem: problem(CodeVersionTooOld, "version", "1.0", "min", "2.0")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"problem":{"code":"agent.version_too_old","params":{"min":"2.0","version":"1.0"}}`) {
		t.Errorf("with a problem: %s", b)
	}
	b, _ = json.Marshal(Installation{Agent: "foo"})
	if strings.Contains(string(b), "problem") {
		t.Errorf("without a problem: %s", b)
	}
}
