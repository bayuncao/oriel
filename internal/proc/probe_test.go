package proc

import (
	"bytes"
	"strings"
	"testing"
)

func TestHeadKeepsBeginning(t *testing.T) {
	h := &head{max: 5}
	for _, s := range []string{"ab", "cdef", "gh"} {
		if n, err := h.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := string(h.Bytes()); got != "abcde" {
		t.Fatalf("head = %q", got)
	}
}

func TestTailKeepsEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes []string
		want   string
	}{
		{"short", []string{"ab", "c"}, "abc"},
		{"exact", []string{"abcde"}, "abcde"},
		{"rolls over", []string{"abc", "def", "gh"}, "defgh"},
		{"compacts", []string{"abcd", "efgh", "ijk", "l"}, "hijkl"},
		{"one big write", []string{"ab", "0123456789"}, "56789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := &tail{max: 5}
			for _, s := range tc.writes {
				if n, err := tl.Write([]byte(s)); n != len(s) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", s, n, err)
				}
				if len(tl.buf) > 2*tl.max {
					t.Fatalf("buffer grew to %d", len(tl.buf))
				}
			}
			if got := string(tl.Bytes()); got != tc.want {
				t.Fatalf("tail = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTailMatchesSuffix 以許多大小不一的寫入比對 tail 與「全部內容的結尾」。
func TestTailMatchesSuffix(t *testing.T) {
	tl := &tail{max: 7}
	var all bytes.Buffer
	for i := range 200 {
		s := strings.Repeat(string(rune('a'+i%26)), i%11)
		_, _ = tl.Write([]byte(s))
		all.WriteString(s)
		want := all.Bytes()[max(0, all.Len()-7):]
		if got := tl.Bytes(); !bytes.Equal(got, want) {
			t.Fatalf("after write %d: %q, want %q", i, got, want)
		}
	}
}
