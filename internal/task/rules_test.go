package task

import "testing"

// TestRuleMatches 檢查單條規則對請求的涵蓋。
func TestRuleMatches(t *testing.T) {
	cases := []struct {
		name  string
		rule  Rule
		agent AgentID
		req   PermissionRequest
		want  bool
	}{
		{"tool match", Rule{Match: MatchTool, Tool: "Read"}, "claude",
			PermissionRequest{Tool: "Read", Kind: ToolRead}, true},
		{"tool mismatch", Rule{Match: MatchTool, Tool: "Read"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute}, false},

		{"prefix equal", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test"}, true},
		{"prefix with space", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test ./..."}, true},
		{"prefix no word boundary", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go testx"}, false},
		{"prefix no space", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test./x"}, false},
		{"prefix empty command", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute}, false},
		{"prefix tool restricted", Rule{Match: MatchPrefix, Tool: "Bash", Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test"}, true},
		{"prefix other tool", Rule{Match: MatchPrefix, Tool: "Bash", Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Edit", Kind: ToolEdit, Command: "go test"}, false},
		{"prefix any tool", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Edit", Kind: ToolEdit, Command: "go test"}, true},

		{"prefix compound &&", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test && rm -rf /"}, false},
		{"prefix compound semicolon", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test; x"}, false},
		{"prefix compound pipe", Rule{Match: MatchPrefix, Pattern: "a"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "a | b"}, false},
		{"prefix compound newline", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test\nrm -rf /"}, false},
		{"prefix compound redirect", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test > out"}, false},
		{"prefix compound backtick", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test `id`"}, false},
		{"prefix compound subst", Rule{Match: MatchPrefix, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test $(id)"}, false},
		{"exact compound allowed", Rule{Match: MatchExact, Pattern: "go test && rm -rf /"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test && rm -rf /"}, true},
		{"exact different", Rule{Match: MatchExact, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test ./..."}, false},
		{"exact empty command", Rule{Match: MatchExact, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute}, false},
		{"exact any tool", Rule{Match: MatchExact, Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Edit", Kind: ToolEdit, Command: "go test"}, true},

		{"agent restricted match", Rule{Agent: "claude", Match: MatchTool, Tool: "Read"}, "claude",
			PermissionRequest{Tool: "Read", Kind: ToolRead}, true},
		{"agent restricted miss", Rule{Agent: "claude", Match: MatchTool, Tool: "Read"}, "codex",
			PermissionRequest{Tool: "Read", Kind: ToolRead}, false},
		{"unknown match", Rule{Match: "bogus", Pattern: "go test"}, "claude",
			PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.matches(tc.agent, tc.req); got != tc.want {
				t.Errorf("matches = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMatchRule 檢查規則選取的優先序與範圍過濾。
func TestMatchRule(t *testing.T) {
	req := PermissionRequest{Tool: "Bash", Kind: ToolExecute, Command: "go test ./..."}
	rule := func(id RuleID, scope RuleScope, scopeID string, v Verdict) Rule {
		return Rule{ID: id, Scope: scope, ScopeID: scopeID, Match: MatchPrefix,
			Tool: "Bash", Pattern: "go test", Verdict: v}
	}
	match := func(state, inputs []Rule) (Rule, bool) {
		d := &decision{
			s: State{ID: "t-1", Workspace: "w1",
				Sessions: map[SessionID]Session{"s2": {ID: "s2", Launch: newLaunch()}},
				Rules:    map[RuleID]Rule{}},
			in: Inputs{Rules: inputs},
		}
		for _, r := range state {
			d.s.Rules[r.ID] = r
		}
		return d.matchRule("s2", req)
	}
	cases := []struct {
		name   string
		state  []Rule
		inputs []Rule
		want   RuleID
		ok     bool
	}{
		{"none", nil, nil, "", false},
		{"state rule", []Rule{rule("r1", ScopeTask, "t-1", Allow)}, nil, "r1", true},
		{"inputs rule", nil, []Rule{rule("w1", ScopeWorkspace, "w1", Allow)}, "w1", true},
		{"combined", []Rule{rule("r1", ScopeTask, "t-1", Allow)},
			[]Rule{rule("w1", ScopeWorkspace, "w1", Allow)}, "r1", true},
		{"deny beats allow across scopes",
			[]Rule{rule("r1", ScopeSession, "s2", Allow)},
			[]Rule{rule("g1", ScopeGlobal, "", Deny)}, "g1", true},
		{"narrower wins",
			[]Rule{rule("r1", ScopeTask, "t-1", Allow), rule("r2", ScopeSession, "s2", Allow)},
			[]Rule{rule("g1", ScopeGlobal, "", Allow), rule("w1", ScopeWorkspace, "w1", Allow)}, "r2", true},
		{"id tiebreak",
			[]Rule{rule("r9", ScopeSession, "s2", Allow), rule("r3", ScopeSession, "s2", Allow)},
			nil, "r3", true},
		{"other session excluded", []Rule{rule("r1", ScopeSession, "s3", Allow)}, nil, "", false},
		{"other workspace excluded", nil, []Rule{rule("w1", ScopeWorkspace, "w2", Allow)}, "", false},
		{"global needs empty scope id", nil, []Rule{rule("g1", ScopeGlobal, "x", Allow)}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := match(tc.state, tc.inputs)
			if ok != tc.ok || got.ID != tc.want {
				t.Errorf("matchRule = %v, %v; want %v, %v", got.ID, ok, tc.want, tc.ok)
			}
		})
	}
}
