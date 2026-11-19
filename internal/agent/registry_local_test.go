//go:build unix

package agent

import (
	"path/filepath"
	"testing"
)

func TestFindInstalled(t *testing.T) {
	m := newMachine(t)
	// npm 全域安裝：PATH 上的命令是指向套件內的符號連結。
	pkg := filepath.Join(m.root, "prefix/lib/node_modules/@google/gemini-cli")
	m.write(filepath.Join(pkg, "package.json"), `{"name":"@google/gemini-cli","version":"0.60.0"}`)
	gemini := m.symlink(m.script(filepath.Join(pkg, "bundle/gemini.js"), ""), filepath.Join(m.path, "gemini"))
	// 名字像但不在套件內的命令不算。
	m.symlink(m.script(filepath.Join(m.root, "elsewhere/pi-acp"), ""), filepath.Join(m.path, "pi-acp"))
	// binary 條目按 cmd 的檔名找。
	cursor := m.script(filepath.Join(m.path, "cursor-agent"), "")
	// uvx 條目不找。
	m.script(filepath.Join(m.path, "fast-agent-acp"), "")

	got := FindInstalled(m.env, m.root, fixtureIndex(t))
	if li := got["gemini"]; li.Path != gemini || li.Version != "0.60.0" {
		t.Errorf("gemini: %+v", li)
	}
	if p, _ := platform(); entry(t, "cursor").Distribution.Binary[p].Cmd != "" {
		if li := got["cursor"]; li.Path != cursor || li.Version != "" {
			t.Errorf("cursor: %+v", li)
		}
	}
	for _, id := range []string{"pi-acp", "fast-agent", "claude-acp", "grok-build"} {
		if li, ok := got[id]; ok {
			t.Errorf("%s found at %+v", id, li)
		}
	}
	for _, s := range []string{gemini, cursor} {
		if runs(s) != 0 {
			t.Errorf("%s was run", s)
		}
	}

	byID := map[string]RegistryListing{}
	for _, l := range ListRegistry(fixtureIndex(t), nil, nil, got) {
		byID[l.Entry.ID] = l
	}
	if byID["gemini"].Installed.Path != gemini || byID["pi-acp"].Installed.Path != "" {
		t.Errorf("listing: %+v / %+v", byID["gemini"], byID["pi-acp"])
	}
}
