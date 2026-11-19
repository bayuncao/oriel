package task

import "testing"

// AcceptsOutput 與 Decide 一致：回傳 false 時 output 事實被拒絕。
func TestAcceptsOutput(t *testing.T) {
	base := fixture() // s2 回合進行中
	noTurn := fixture()
	x := noTurn.Sessions["s2"]
	x.Turn = nil
	noTurn.Sessions["s2"] = x
	closed := fixture()
	x = closed.Sessions["s2"]
	x.Status = SessionClosed
	closed.Sessions["s2"] = x

	for _, tc := range []struct {
		name string
		s    State
		sid  SessionID
		want bool
	}{
		{"unknown session", base, "s-none", false},
		{"closed session", closed, "s2", false},
		{"no turn", noTurn, "s2", false},
		{"turn in progress", base, "s2", true},
	} {
		if got := AcceptsOutput(tc.s, tc.sid); got != tc.want {
			t.Errorf("%s: AcceptsOutput = %v", tc.name, got)
		}
		// Decide 對同一前提的判斷必須一致。
		_, err := Decide(tc.s, Inputs{}, Command{
			Kind: FactOutput, Session: tc.sid, Origin: OriginRuntime,
			Output: &Output{Channel: ChannelText, Text: "x"}})
		if (err == nil) != tc.want {
			t.Errorf("%s: Decide err %v", tc.name, err)
		}
	}
}
