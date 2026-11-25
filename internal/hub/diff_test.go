package hub

import (
	"reflect"
	"strings"
	"testing"
)

// m4.md WT01–WT04：parseDiff 逐檔拆開 git diff 的輸出。
func TestParseDiff(t *testing.T) {
	const text = `diff --git a/src/b.go b/src/b.go
index 1111111..2222222 100644
--- a/src/b.go
+++ b/src/b.go
@@ -1,2 +1,2 @@
 package b
-var x = 1
+var x = 2
diff --git a/new file.txt b/new file.txt
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/new file.txt	
@@ -0,0 +1 @@
+hi
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 4444444..0000000
--- a/gone.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-a
-b
diff --git a/old.go b/moved.go
similarity index 90%
rename from old.go
rename to moved.go
index 5555555..6666666 100644
--- a/old.go
+++ b/moved.go
@@ -1 +1 @@
-x
+y
diff --git a/logo.png b/logo.png
new file mode 100644
index 0000000..7777777
Binary files /dev/null and b/logo.png differ
diff --git "a/\344\270\255.txt" "b/\344\270\255.txt"
new file mode 100644
index 0000000..8888888
--- /dev/null
+++ "b/\344\270\255.txt"
@@ -0,0 +1 @@
+z
diff --git a/run.sh b/run.sh
old mode 100644
new mode 100755
`
	v := parseDiff(text, 1<<20)
	type row struct {
		Path, OldPath   string
		Change          FileChange
		Added, Removed  int
		Binary, Omitted bool
		Patch           bool
	}
	var got []row
	for _, f := range v.Files {
		got = append(got, row{f.Path, f.OldPath, f.Change, f.Added, f.Removed, f.Binary, f.Omitted, f.Patch != ""})
	}
	want := []row{ // 照 git 輸出的順序
		{"src/b.go", "", ChangeModified, 1, 1, false, false, true},
		{"new file.txt", "", ChangeAdded, 1, 0, false, false, true},
		{"gone.txt", "", ChangeDeleted, 0, 2, false, false, true},
		{"moved.go", "old.go", ChangeRenamed, 1, 1, false, false, true},
		{"logo.png", "", ChangeAdded, 0, 0, true, false, false},
		{"中.txt", "", ChangeAdded, 1, 0, false, false, true},
		{"run.sh", "", ChangeModified, 0, 0, false, false, false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files:\n got %+v\nwant %+v", got, want)
	}
	if v.Omitted {
		t.Fatal("nothing should be omitted")
	}
	b := v.Files[0].Patch
	if !strings.HasPrefix(b, "@@ -1,2 +1,2 @@\n") || !strings.HasSuffix(b, "+var x = 2\n") {
		t.Fatalf("patch = %q", b)
	}

	t.Run("WT02 empty", func(t *testing.T) {
		if v := parseDiff("", 10); v.Files == nil || len(v.Files) != 0 || v.Omitted {
			t.Fatalf("parseDiff(\"\") = %+v", v)
		}
	})
	t.Run("WT04 over the limit", func(t *testing.T) {
		// src/b.go 的 patch 放得下，接著的 new file.txt 放不下：它與之後有 patch 的都只留行數。
		limit := len(v.Files[0].Patch) + 1
		v := parseDiff(text, limit)
		if !v.Omitted {
			t.Fatal("Omitted = false")
		}
		for i, f := range v.Files {
			wantOmitted := i >= 1 && !f.Binary && f.Path != "run.sh"
			if f.Omitted != wantOmitted || (f.Patch != "") != (i == 0) {
				t.Errorf("%s: omitted %v, patch %q", f.Path, f.Omitted, f.Patch)
			}
			if f.Path == "moved.go" && (f.Added != 1 || f.Removed != 1) {
				t.Errorf("moved.go lost its counts: %+v", f)
			}
		}
	})
}
