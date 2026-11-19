package task

import "fmt"

// reviewPrompt 是審查者 Session 的首個 Prompt（D17）：任務說明、候選名單、
// 要求最後一行寫 "WINNER: <session id>"；各 Fork 的 diff 作為附件。
// hub 依最後一行擷取 recommendation。
func reviewPrompt(s State, candidates []SessionID, diffs []ForkDiff) Prompt {
	var b []byte
	b = fmt.Appendf(b, "You are reviewing %d independent attempts at the same task.\n\n", len(candidates))
	b = fmt.Appendf(b, "Task: %s\n\n", s.Title)
	if s.Brief != "" {
		b = fmt.Appendf(b, "%s\n\n", s.Brief)
	}
	b = fmt.Appendf(b, "Candidates:\n")
	attachments := make([]Attachment, len(diffs))
	for i, sid := range candidates {
		l := s.Sessions[sid].Launch
		b = fmt.Appendf(b, "- %s: %s (%s) — attachment %s\n", sid, l.AgentName, l.ProfileName, diffs[i].Diff.Name)
		attachments[i] = diffs[i].Diff
	}
	b = fmt.Appendf(b, "\nCompare the diffs for correctness, completeness and code quality. Explain your reasoning, then end your reply with exactly one final line of the form:\nWINNER: <session id>")
	return Prompt{Text: string(b), Attachments: attachments}
}
