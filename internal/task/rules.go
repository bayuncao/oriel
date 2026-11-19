package task

import (
	"cmp"
	"slices"
	"strings"
)

// scopeRank 是規則範圍由窄到寬的順序；多條命中時窄的優先。
var scopeRank = map[RuleScope]int{ScopeSession: 0, ScopeTask: 1, ScopeWorkspace: 2, ScopeGlobal: 3}

// matchRule 在 Session 與 Task 範圍（State.Rules）和 Workspace 與全域範圍（Inputs.Rules）的規則中
// 找出第一條命中 req 的：拒絕優先於允許，再按範圍由窄到寬，再按 ID；結果確定、可重放。
// 範圍不對（別的 Session、別的 Workspace）的規則不參與。
func (d *decision) matchRule(sid SessionID, req PermissionRequest) (Rule, bool) {
	var rs []Rule
	for _, r := range d.s.Rules {
		if d.applies(r, sid) {
			rs = append(rs, r)
		}
	}
	for _, r := range d.in.Rules {
		if d.applies(r, sid) {
			rs = append(rs, r)
		}
	}
	slices.SortFunc(rs, func(a, b Rule) int {
		if a.Verdict != b.Verdict {
			if a.Verdict == Deny {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(scopeRank[a.Scope], scopeRank[b.Scope]); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	agent := d.s.Sessions[sid].Launch.Agent
	for _, r := range rs {
		if r.matches(agent, req) {
			return r, true
		}
	}
	return Rule{}, false
}

// applies 回報規則的範圍是否涵蓋這個 Session 與 Task；範圍不對的不參與。
func (d *decision) applies(r Rule, sid SessionID) bool {
	id, ok := d.scopeID(r.Scope, sid)
	return ok && r.ScopeID == id
}

// scopeID 回報範圍的對象 ID；未知範圍為 false。
func (d *decision) scopeID(scope RuleScope, sid SessionID) (string, bool) {
	switch scope {
	case ScopeSession:
		return string(sid), true
	case ScopeTask:
		return string(d.s.ID), true
	case ScopeWorkspace:
		return string(d.s.Workspace), true
	case ScopeGlobal:
		return "", true
	}
	return "", false
}

// matches 回報規則是否涵蓋這個請求。前綴按詞邊界比對；含有 shell 控制字元的複合命令
// （換行、; | & > < ` $(）不由前綴規則自動放行，只能完全匹配或由使用者決定。
func (r Rule) matches(agent AgentID, req PermissionRequest) bool {
	if r.Agent != "" && r.Agent != agent {
		return false
	}
	tool := r.Tool == "" || r.Tool == req.Tool
	switch r.Match {
	case MatchTool:
		return r.Tool == req.Tool
	case MatchPrefix:
		return tool && req.Command != "" && !compound(req.Command) && hasWordPrefix(req.Command, r.Pattern)
	case MatchExact:
		return tool && req.Command != "" && req.Command == r.Pattern
	}
	return false
}

// hasWordPrefix 回報 s 等於 p 或以 p 加空白開頭（"go test" 不含 "go testx"）。
func hasWordPrefix(s, p string) bool {
	return s == p || strings.HasPrefix(s, p+" ")
}

// compound 回報命令是否含有 shell 控制字元。
func compound(s string) bool {
	return strings.ContainsAny(s, "\n;|&><`") || strings.Contains(s, "$(")
}

// checkAlways 校驗「總是允許 / 拒絕」要存的規則並補齊欄位：Verdict 取這次決定的，Tool 為空時取請求的；
// 有 Ask 時規則必須涵蓋這個請求，命令類請求（execute）不能整個工具放行；remote 沒有 Ask 就不能建規則。
func (d *decision) checkAlways(r Rule, verdict Verdict, ask *Ask, agent AgentID, origin Origin) (Rule, error) {
	switch r.Scope {
	case ScopeSession, ScopeTask, ScopeWorkspace, ScopeGlobal:
	default:
		return Rule{}, invalid("unknown rule scope %q", r.Scope)
	}
	switch r.Match {
	case MatchTool, MatchPrefix, MatchExact:
	default:
		return Rule{}, invalid("unknown rule match %q", r.Match)
	}
	r.Pattern = strings.TrimSpace(r.Pattern)
	if r.Tool == "" && ask != nil {
		r.Tool = ask.Request.Tool
	}
	if r.Match == MatchTool && r.Tool == "" {
		return Rule{}, invalid("tool rule needs a tool")
	}
	if r.Match != MatchTool && r.Pattern == "" {
		return Rule{}, invalid("rule needs a pattern")
	}
	r.Verdict, r.ID, r.ScopeID = verdict, "", ""
	if ask == nil {
		if origin == OriginRemote {
			return Rule{}, invalid("rule needs a pending ask")
		}
		return r, nil
	}
	if r.Match == MatchTool && ask.Request.Kind == ToolExecute {
		return Rule{}, invalid("commands need a prefix or exact rule")
	}
	if r.Tool != "" && r.Tool != ask.Request.Tool {
		return Rule{}, invalid("rule tool differs from request")
	}
	if !r.matches(agent, ask.Request) {
		return Rule{}, invalid("rule does not cover the request")
	}
	return r, nil
}

// addRule 填上 ScopeID 與 ID 並記錄規則；與既有規則（State.Rules 或 Inputs.Rules）內容相同時不重複。
func (d *decision) addRule(sid SessionID, r Rule) {
	r.ScopeID, _ = d.scopeID(r.Scope, sid)
	dup := func(e Rule) bool {
		return e.Scope == r.Scope && e.ScopeID == r.ScopeID && e.Agent == r.Agent &&
			e.Match == r.Match && e.Tool == r.Tool && e.Pattern == r.Pattern && e.Verdict == r.Verdict
	}
	for _, e := range d.s.Rules {
		if dup(e) {
			return
		}
	}
	for _, e := range d.in.Rules {
		if dup(e) {
			return
		}
	}
	id := ruleID(d.next())
	if r.Scope == ScopeWorkspace || r.Scope == ScopeGlobal {
		id = RuleID(string(d.s.ID) + ":" + string(id))
	}
	r.ID = id
	d.emit(Event{Kind: RuleAddedKind, RuleAdded: &r})
}
