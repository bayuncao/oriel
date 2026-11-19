package hub

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// m2.md B07–B10（D135）：新建與改標籤的規則。
func TestPutLabel(t *testing.T) {
	env := newTestHub(t, nil)
	front, err := env.h.PutLabel(ctxb(), LocalOwner, Label{Name: "  前端 ", Tone: ToneSky})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(front.ID), "l-") || front.Name != "前端" || front.Tone != ToneSky {
		t.Fatalf("B07 created %+v", front)
	}
	back, err := env.h.PutLabel(ctxb(), LocalOwner, Label{Name: "Backend", Tone: ToneAmber})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		label Label
		code  Code
		param string // 錯誤的參數 field、name 或 label 的值
	}{
		{"B08 same name", Label{Name: "前端", Tone: ToneTeal}, CodeDuplicateLabel, "前端"},
		{"B08 same name with spaces", Label{Name: " 前端  ", Tone: ToneTeal}, CodeDuplicateLabel, "前端"},
		{"B08 same name in another case", Label{Name: "BACKEND", Tone: ToneTeal}, CodeDuplicateLabel, "Backend"},
		{"B10 rename to another label's name", Label{ID: front.ID, Name: "backend", Tone: ToneSky}, CodeDuplicateLabel, "Backend"},
		{"B09 empty name", Label{Name: "", Tone: ToneTeal}, CodeInvalidField, "name"},
		{"B09 only spaces", Label{Name: "   ", Tone: ToneTeal}, CodeInvalidField, "name"},
		{"B09 two lines", Label{Name: "a\nb", Tone: ToneTeal}, CodeInvalidField, "name"},
		{"B09 41 characters", Label{Name: strings.Repeat("标", 41), Tone: ToneTeal}, CodeInvalidField, "name"},
		{"B07 unknown tone", Label{Name: "x", Tone: "blue"}, CodeInvalidField, "tone"},
		{"B07 no tone", Label{Name: "x"}, CodeInvalidField, "tone"},
		{"B10 unknown label", Label{ID: "l-gone", Name: "x", Tone: ToneTeal}, CodeUnknownLabel, "l-gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.h.PutLabel(ctxb(), LocalOwner, tc.label)
			he, ok := err.(*Error)
			if !ok || he.Code != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
			if !slicesContainsValue(he.Params, tc.param) {
				t.Fatalf("params %v, want %q", he.Params, tc.param)
			}
		})
	}
	long, err := env.h.PutLabel(ctxb(), LocalOwner, Label{Name: strings.Repeat("标", 40), Tone: ToneGreen})
	if err != nil {
		t.Fatalf("B09 40 characters: %v", err)
	}
	// B10：改名（含只改大小寫）與改色調。
	front.Name, front.Tone = "FRONT", ToneRose
	if _, err := env.h.PutLabel(ctxb(), LocalOwner, front); err != nil {
		t.Fatal(err)
	}
	front.Name = "Front"
	if _, err := env.h.PutLabel(ctxb(), LocalOwner, front); err != nil {
		t.Fatalf("B10 own name in another case: %v", err)
	}
	want := sortedLabels(map[task.LabelID]Label{front.ID: front, back.ID: back, long.ID: long})
	if got, _ := env.h.Labels(ctxb(), LocalOwner); !reflect.DeepEqual(got, want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
	if got, _ := env.store.Labels(ctxb()); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored %v, want %v", got, want)
	}
	// 重啟後從 Config 載回。
	env.h.Close()
	again := newTestHub(t, func(o *Options) { o.Log, o.Config, o.Index = env.store, env.store, env.store })
	if got, _ := again.h.Labels(ctxb(), LocalOwner); !reflect.DeepEqual(got, want) {
		t.Fatalf("after restart %v, want %v", got, want)
	}
}

func slicesContainsValue(m map[string]string, v string) bool {
	for _, x := range m {
		if x == v {
			return true
		}
	}
	return false
}

// 遠端不能改標籤目錄，讀可以。
func TestLabelsRemote(t *testing.T) {
	env := newTestHub(t, nil)
	l, _ := env.h.PutLabel(ctxb(), LocalOwner, Label{Name: "a", Tone: ToneTeal})
	remote := Principal{Device: "d9"}
	if _, err := env.h.PutLabel(ctxb(), remote, Label{Name: "b", Tone: ToneTeal}); errCode(err) != string(CodeNeedsDesktop) {
		t.Fatalf("put %v", err)
	}
	if err := env.h.DeleteLabel(ctxb(), remote, l.ID); errCode(err) != string(CodeNeedsDesktop) {
		t.Fatalf("delete %v", err)
	}
	if got, err := env.h.Labels(ctxb(), remote); err != nil || len(got) != 1 {
		t.Fatalf("read %v %v", got, err)
	}
}

// m2.md B02、B05、B11、B12、B14、B15：經 hub 改優先級與標籤，Task 列帶著它們與 agent。
func TestTaskPriorityAndLabels(t *testing.T) {
	env := newTestHub(t, nil)
	plain := env.create(t, t.TempDir(), "", testLaunch())
	withAgent := env.create(t, t.TempDir(), "go", testLaunch())
	front, _ := env.h.PutLabel(ctxb(), LocalOwner, Label{Name: "front", Tone: ToneSky})
	back, _ := env.h.PutLabel(ctxb(), LocalOwner, Label{Name: "back", Tone: ToneAmber})
	row := func(id task.TaskID) TaskView {
		t.Helper()
		rows, err := env.h.Tasks(ctxb(), LocalOwner)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == id {
				return r
			}
		}
		t.Fatalf("no row for %s", id)
		return TaskView{}
	}
	before := row(plain)
	if before.Priority != task.PriorityNone || len(before.Labels) != 0 || before.Labels == nil || len(before.Agents) != 0 || before.Agents == nil {
		t.Fatalf("B01、B14 new task %+v", before)
	}
	if got := row(withAgent).Agents; !reflect.DeepEqual(got, []TaskAgent{{ID: "test", Name: "Test Agent"}}) {
		t.Fatalf("B15 agents %v", got)
	}
	env.clock.Advance(time.Minute)
	if _, err := env.h.Command(ctxb(), LocalOwner, plain, task.Command{Kind: task.CmdSetPriority, SetPriority: &task.SetPriority{Priority: task.PriorityUrgent}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.h.Command(ctxb(), LocalOwner, plain, task.Command{Kind: task.CmdSetLabels, SetLabels: &task.SetLabels{Labels: []task.LabelID{front.ID, back.ID}}}); err != nil {
		t.Fatal(err)
	}
	after := row(plain)
	if after.Priority != task.PriorityUrgent || !reflect.DeepEqual(after.Labels, sortedIDs(front.ID, back.ID)) {
		t.Fatalf("B02、B12 row %+v", after)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("B05 updatedAt %v, want %v", after.UpdatedAt, before.UpdatedAt)
	}
	// B11：刪掉的標籤不能再設；之後同名的是新標籤，不回到舊 Task 上。
	if err := env.h.DeleteLabel(ctxb(), LocalOwner, back.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.h.DeleteLabel(ctxb(), LocalOwner, back.ID); errCode(err) != string(CodeUnknownLabel) {
		t.Fatalf("delete twice %v", err)
	}
	_, err := env.h.Command(ctxb(), LocalOwner, plain, task.Command{Kind: task.CmdSetLabels, SetLabels: &task.SetLabels{Labels: []task.LabelID{front.ID, back.ID}}})
	if errCode(err) != "task.unknown_label" {
		t.Fatalf("B12 deleted label %v", err)
	}
	again, err := env.h.PutLabel(ctxb(), LocalOwner, Label{Name: "back", Tone: ToneAmber})
	if err != nil || again.ID == back.ID {
		t.Fatalf("recreated %+v %v", again, err)
	}
	if got := row(plain).Labels; slices.Contains(got, again.ID) {
		t.Fatalf("B11 new label on the old task %v", got)
	}
}

func sortedIDs(ids ...task.LabelID) []task.LabelID {
	return slices.Sorted(slices.Values(ids))
}
