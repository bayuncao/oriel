package hub

// attach_test.go：附件只能是 Prompt 送去的工作目錄裡的檔案（D100，m2.md V45），hub 在 Actor 裡核對。

import (
	"path/filepath"
	"testing"

	"github.com/bayuncao/oriel/internal/task"
)

func TestAttachmentsInsideWorkdir(t *testing.T) {
	env := newTestHub(t, func(o *Options) { o.Launcher = newDiscoveryLauncher(t) })
	dir := t.TempDir()
	id := env.create(t, dir, "say ok", testLaunch())
	watch(t, env.h, id).next(task.TurnCompletedKind)
	snap, err := env.h.Snapshot(ctxb(), LocalOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := env.h.workspaceOf(snap.Workspace)
	cases := []struct {
		name string
		att  task.Attachment
		ok   bool
	}{
		{"inside", task.Attachment{Name: "a.go", Path: filepath.Join(w.Path, "a.go")}, true},
		{"blob", task.Attachment{Name: "shot.png", MIME: "image/png", Blob: "b1"}, true},
		{"outside", task.Attachment{Name: "hosts", Path: filepath.FromSlash("/etc/hosts")}, false},
		{"sibling with a shared prefix", task.Attachment{Name: "a.go", Path: w.Path + "x" + string(filepath.Separator) + "a.go"}, false},
		{"climbs out", task.Attachment{Name: "a.go", Path: filepath.Join(w.Path, "..", "a.go")}, false},
		{"relative", task.Attachment{Name: "a.go", Path: "a.go"}, false},
		{"the workdir itself", task.Attachment{Name: "dir", Path: w.Path}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := env.h.Command(ctxb(), LocalOwner, id, task.Command{Kind: task.CmdSubmit, Session: snap.Order[0],
				Submit: &task.Submit{Prompt: task.Prompt{Text: "see", Attachments: []task.Attachment{c.att}}}})
			if c.ok != (err == nil) {
				t.Fatalf("err %v, want ok %v", err, c.ok)
			}
			if !c.ok {
				if r, isRej := err.(*task.Rejection); !isRej || r.Reason != task.ReasonInvalid {
					t.Fatalf("err %v, want invalid", err)
				}
			}
		})
	}
	// 啟動 agent 的第一個 Prompt 也核對。
	bare := env.create(t, dir, "", task.LaunchSnapshot{})
	_, err = env.h.CreateSessionWithAgent(ctxb(), LocalOwner, bare, "test", task.Prompt{Text: "see",
		Attachments: []task.Attachment{{Name: "hosts", Path: filepath.FromSlash("/etc/hosts")}}})
	if r, isRej := err.(*task.Rejection); !isRej || r.Reason != task.ReasonInvalid {
		t.Fatalf("starting an agent with an outside attachment: %v, want invalid", err)
	}
}
