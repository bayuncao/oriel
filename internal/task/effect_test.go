package task

import "testing"

func TestEffectKey(t *testing.T) {
	if got := newEffectKey(EffPrepareReview, ""); got != "prepare_review" {
		t.Errorf("newEffectKey(prepare_review, \"\") = %q", got)
	}
	if got := newEffectKey(EffMergeWorktree, "i21"); got != "merge_worktree:i21" {
		t.Errorf("newEffectKey(merge_worktree, i21) = %q", got)
	}
	for _, tc := range []struct {
		kind IntentKind
		want EffectKey
	}{
		{IntentMerge, "merge_worktree:i21"},
		{IntentDiscard, "discard_worktree:i21"},
		{IntentDeleteNative, "delete_native:i21"},
	} {
		got := Intent{ID: "i21", Kind: tc.kind}.effectKey()
		if got != tc.want || got.Kind() != tc.want.Kind() {
			t.Errorf("Intent{Kind: %s}.effectKey() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestEffectKeyKind(t *testing.T) {
	for _, tc := range []struct {
		key  EffectKey
		want EffectKind
	}{
		{"start_conn:s2", EffStartConn},
		{"merge_worktree:i21", EffMergeWorktree},
		{"prepare_review", EffPrepareReview},
		{"", ""},
	} {
		if got := tc.key.Kind(); got != tc.want {
			t.Errorf("%q.Kind() = %q, want %q", tc.key, got, tc.want)
		}
	}
}
