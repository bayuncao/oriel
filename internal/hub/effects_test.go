package hub

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// effectKinds 讀 task/effect.go 頂層宣告的 EffectKind 常數：值 → 名稱。
func effectKinds(t *testing.T) map[task.EffectKind]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../task/effect.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[task.EffectKind]string{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "EffectKind" {
				continue
			}
			if len(vs.Values) != len(vs.Names) {
				t.Fatalf("%s: %d names, %d values", vs.Names[0].Name, len(vs.Names), len(vs.Values))
			}
			for i, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not a string literal", vs.Names[i].Name)
				}
				k, _ := strconv.Unquote(lit.Value)
				kinds[task.EffectKind(k)] = vs.Names[i].Name
			}
		}
	}
	if len(kinds) == 0 {
		t.Fatal("no EffectKind constants found in task/effect.go")
	}
	return kinds
}

// 每種 Effect 都有分派它的方法，只有 report_exited 例外：它由 plan 就地處理（D117），
// 有方法就會被分派第二次。
func TestEffectorsCoverEffects(t *testing.T) {
	for k, name := range effectKinds(t) {
		if got, want := effectorOf(k) != nil, k != task.EffReportExited; got != want {
			t.Errorf("%s: has effector = %v, want %v", name, got, want)
		}
	}
}

// bareActor 是不跑迴圈的 actor：dispatch 的結果留在郵箱裡給測試讀，日誌寫進 log。
// 結束時等它 spawn 的 goroutine 都回來。
func bareActor(t *testing.T, log *bytes.Buffer) *actor {
	h := &Hub{ctx: t.Context(), logger: slog.New(slog.NewTextHandler(log, nil))}
	a := newActor(h, "t", task.State{}, task.Live{}, nil)
	t.Cleanup(func() {
		close(a.stop)
		a.inflight.Wait()
	})
	return a
}

// failure 讀 dispatch 送進郵箱的結果，它必須是 key 的 effect_failed。
func failure(t *testing.T, a *actor, key task.EffectKey) task.Problem {
	t.Helper()
	select {
	case m := <-a.mail:
		if m.done == nil || m.done.key != key || len(m.done.facts) != 1 || m.done.facts[0].EffectFailed == nil {
			t.Fatalf("got %+v, want effect_failed for %s", m.done, key)
		}
		return m.done.facts[0].EffectFailed.Problem
	case <-time.After(5 * time.Second):
		t.Fatalf("no result for %s", key)
	}
	panic("unreachable")
}

// 沒有方法的 Kind 記一條錯誤日誌，留在 InFlight：不 spawn，Plan 再跑也不會再分派它。
func TestDispatchUnknownKind(t *testing.T) {
	var log bytes.Buffer
	a := bareActor(t, &log)
	a.dispatch(task.Effect{Kind: "made_up", Key: "made_up:s1", Session: "s1"})
	if !strings.Contains(log.String(), "level=ERROR") || !strings.Contains(log.String(), "kind=made_up") {
		t.Fatalf("log = %q, want an error naming made_up", log.String())
	}
	select {
	case m := <-a.mail:
		t.Fatalf("dispatch sent %+v, want nothing", m)
	default:
	}
}

// 要連線的 Effect 在 Session 沒有連線時都回報 connection_lost。
func TestConnEffectsWithoutConn(t *testing.T) {
	for _, e := range []task.Effect{
		{Kind: task.EffSendPrompt, SendPrompt: &task.Prompt{}},
		{Kind: task.EffCancelTurn}, // 沒有回合，送出的回合也是空的：兩者相等，算 Prompt 已送出
		{Kind: task.EffAnswerPermission, AnswerPermission: &task.AnswerPermission{Permission: "p1"}},
		{Kind: task.EffSetModel, SetModel: &task.ModelSwitch{Option: "model", Value: "m"}},
	} {
		t.Run(string(e.Kind), func(t *testing.T) {
			a := bareActor(t, new(bytes.Buffer))
			e.Session, e.Key = "s1", task.EffectKey(string(e.Kind)+":s1")
			a.dispatch(e)
			if p := failure(t, a, e.Key); p.Code != string(CodeConnectionLost) {
				t.Fatalf("problem = %+v, want %s", p, CodeConnectionLost)
			}
		})
	}
}
