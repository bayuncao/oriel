package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bayuncao/oriel/internal/updater"
)

// TestReleaseNotesFiles 檢查 release-notes/ 裡每一份更新說明（m2.md 第 27 節，D142）：
// 檔名是版本號，內容的 version 與它相同，英文那一份不是空的（界面語言沒有時用它），不超過應用肯讀的大小。
func TestReleaseNotesFiles(t *testing.T) {
	files, err := filepath.Glob("release-notes/*.json")
	if err != nil {
		t.Fatal(err)
	}
	version := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			if !version.MatchString(name) {
				t.Errorf("%s: the file name is not a version (x.y.z)", f)
			}
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) > updater.MaxReleaseNotes {
				t.Errorf("%s: larger than %d bytes", f, updater.MaxReleaseNotes)
			}
			var notes updater.ReleaseNotes
			dec := json.NewDecoder(strings.NewReader(string(data)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&notes); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if notes.Version != name {
				t.Errorf("%s: version is %q", f, notes.Version)
			}
			if strings.TrimSpace(notes.Notes["en"]) == "" {
				t.Errorf("%s: no English notes", f)
			}
		})
	}
}
