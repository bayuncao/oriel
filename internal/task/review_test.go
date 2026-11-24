package task

import (
	"reflect"
	"strings"
	"testing"
)

func TestReviewPrompt(t *testing.T) {
	s := State{Title: "Login page", Brief: "Build it",
		Sessions: map[SessionID]Session{
			"s2": {ID: "s2", Launch: newLaunch()},
			"s3": {ID: "s3", Launch: LaunchSnapshot{AgentName: "Codex", Profile: "p9", ProfileName: "cli"}},
		}}
	diff := func(name string) Attachment { return Attachment{Name: name, MIME: "text/x-diff"} }
	diffs := []ForkDiff{
		{Session: "s2", Diff: diff("s2.diff")},
		{Session: "s3", Diff: diff("s3.diff")},
	}
	p := reviewPrompt(s, []SessionID{"s2", "s3"}, diffs)
	want := "You are reviewing 2 independent attempts at the same task.\n\n" +
		"Task: Login page\n\nBuild it\n\nCandidates:\n" +
		"- s2: Claude Code (default) — attachment s2.diff\n" +
		"- s3: Codex (cli) — attachment s3.diff\n" +
		"\nCompare the diffs for correctness, completeness and code quality. " +
		"Explain your reasoning, then end your reply with exactly one final line of the form:\n" +
		"WINNER: <session id>"
	if p.Text != want {
		t.Errorf("text =\n%s\nwant\n%s", p.Text, want)
	}
	if !reflect.DeepEqual(p.Attachments, []Attachment{diff("s2.diff"), diff("s3.diff")}) {
		t.Errorf("attachments %v", p.Attachments)
	}
	s.Brief = ""
	if got := reviewPrompt(s, []SessionID{"s2", "s3"}, diffs).Text; !strings.Contains(got, "Task: Login page\n\nCandidates:\n") {
		t.Errorf("empty brief leaves blank lines:\n%s", got)
	}
}
