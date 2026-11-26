package task

import (
	"cmp"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Handoff 是接力時交接的來源（D83，m4.md 第 4 節）：前一個 Session，與 hub 讀好的 Task 改動。
type Handoff struct {
	From    SessionID `json:"from"`
	Changes Changes   `json:"changes"`
}

// Changes 是交接時 Task 的改動（HO05、HO08–HO11）。只列檔案與行數，完整的改動由 agent 自己用 git 讀。
type Changes struct {
	// Worktree：改動是 Task Worktree 相對 BaseCommit 的（有行數）；否則是 Task 的 Session 改過的檔案（沒有行數）。
	Worktree bool `json:"worktree"`
	// Git：Task 直接在資料夾裡時，資料夾是不是 git 倉庫（決定附不附 git 命令）。
	Git bool `json:"git"`
	// Unread：沒能讀到改動（HO11）；Files 為空。
	Unread bool          `json:"unread"`
	Files  []ChangedFile `json:"files"`
	// More 是 Files 之外還有幾個（最多列 MaxHandoffFiles 個，HO09）。
	More int `json:"more"`
}

// ChangedFile 是交接內容裡的一個檔案。Change 是 added / modified / deleted / renamed，
// 沒有行數時（Task 直接在資料夾裡）為空。
type ChangedFile struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitzero"`
	Change  string `json:"change,omitzero"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary"`
}

// MaxHandoffFiles 是交接內容最多列的檔案數（HO09）；MaxHandoffReply 是帶上的回覆的位元組上限（HO06）。
const (
	MaxHandoffFiles = 200
	MaxHandoffReply = 32 << 10
)

// HandoffSource 是接力的來源：這個 Task 的、不是 Fork 的 Session（HO04）。
func HandoffSource(s State, id SessionID) (Session, error) {
	x, ok := s.Sessions[id]
	if !ok || x.Fork != nil {
		return Session{}, reject(ReasonUnknownSession)
	}
	return x, nil
}

// CanStartSession 是在 Task 資料夾裡再開 Session 的前提（HO03、HO12、HO13）：未歸檔、
// 資料夾裡沒有 Session 在運行、Task Worktree 沒有處置也沒有處置的意圖。hub 在解析 agent 前先用它擋，
// Decide 在 Actor 裡再查一次（HO17）。
func CanStartSession(s State) error {
	if s.Archived {
		return reject(ReasonArchived)
	}
	if busy(s, "") {
		return reject(ReasonWorkdirBusy)
	}
	if s.WorktreeClosing() {
		return reject(ReasonWorktreeClosed)
	}
	for _, x := range s.Sessions {
		if x.openFork() {
			return reject(ReasonUnresolvedChanges) // 扇出還沒選定：先選定或丟棄 Fork（HO12）
		}
	}
	return nil
}

// busy 回報 Task 資料夾裡（不算 Fork）除了 except 之外有沒有 Session 在運行（決定 2）。
func busy(s State, except SessionID) bool {
	for id, x := range s.Sessions {
		if id != except && x.Fork == nil && x.Status == SessionRunning {
			return true
		}
	}
	return false
}

// handoffContext 是新 Session 第一個 Prompt 的交接內容（HO05–HO11）：英文，只放摘要與引用。
func handoffContext(s State, from Session, c Changes) string {
	var b []byte
	b = fmt.Appendf(b, "Handoff from OrielHub: you are taking over a task that another agent session has been working on in this folder.\n\n")
	b = fmt.Appendf(b, "Task: %s\n", s.Title)
	if s.Brief != "" {
		b = fmt.Appendf(b, "\n%s\n", s.Brief)
	}
	l := from.Launch
	b = fmt.Appendf(b, "\nPrevious session: %s", cmp.Or(l.AgentName, string(l.Agent)))
	if l.ProfileName != "" {
		b = fmt.Appendf(b, " (profile %s)", l.ProfileName)
	}
	b = fmt.Appendf(b, ", session %s\n", from.ID)

	var reply *Reply
	if from.LastTurn != nil {
		reply = from.LastTurn.Reply
	}
	switch {
	case reply == nil:
		b = fmt.Appendf(b, "\nThe previous session's last turn ended without a reply.\n")
	case reply.Redacted:
		b = fmt.Appendf(b, "\nThe previous session's last reply (message %s) was deleted by the user.\n", reply.Message)
	default:
		text, cut := truncate(reply.Text, MaxHandoffReply)
		// 引文裡的結束標記會讓後面的文字跑出引文、像是 OrielHub 的指示：換掉它。
		text = strings.ReplaceAll(text, "</previous-reply>", "<\\/previous-reply>")
		b = fmt.Appendf(b, "\nIts last reply (message %s):\n\n<previous-reply>\n%s\n</previous-reply>\n", reply.Message, text)
		if cut {
			b = fmt.Appendf(b, "(The reply was truncated to %d KiB.)\n", MaxHandoffReply>>10)
		}
	}
	b = append(b, '\n')
	return string(appendChanges(b, s, c))
}

func appendChanges(b []byte, s State, c Changes) []byte {
	w := s.Worktree
	worktree := c.Worktree && w != nil
	if worktree {
		b = fmt.Appendf(b, "Changes so far: worktree branch %s, based on %s at commit %s.\n", w.Branch, w.BaseBranch, w.BaseCommit)
	} else {
		b = fmt.Appendf(b, "Changes so far: files this task's sessions edited in this folder.\n")
	}
	switch {
	case c.Unread:
		b = fmt.Appendf(b, "OrielHub could not read the changes.\n")
	case len(c.Files) == 0:
		b = fmt.Appendf(b, "No changes yet.\n")
	}
	for _, f := range c.Files {
		b = appendFile(b, f)
	}
	if c.More > 0 {
		b = fmt.Appendf(b, "… and %d more files.\n", c.More)
	}
	switch {
	case worktree:
		b = fmt.Appendf(b, "Run `git diff %s` to see the full changes.\n", w.BaseCommit)
	case c.Git:
		b = fmt.Appendf(b, "Run `git status` and `git diff` to see the changes.\n")
	}
	return b
}

func appendFile(b []byte, f ChangedFile) []byte {
	if f.Change == "" {
		return fmt.Appendf(b, "- %s\n", f.Path)
	}
	b = fmt.Appendf(b, "- %s %s", f.Change, f.Path)
	if f.OldPath != "" {
		b = fmt.Appendf(b, " (from %s)", f.OldPath)
	}
	if f.Binary {
		return fmt.Appendf(b, " (binary)\n")
	}
	return fmt.Appendf(b, " (+%d -%d)\n", f.Added, f.Removed)
}

// truncate 把 s 截到最多 n 個位元組，不切開 UTF-8 字元。
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
