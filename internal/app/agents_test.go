package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/proc"
)

// TestBuiltinVersionPatterns 將每個 builtin 的版本 pattern
// 對照從真實 CLI 錄下的輸出，以及同名其他程式的輸出
// 進行檢查。
func TestBuiltinVersionPatterns(t *testing.T) {
	type sample struct{ match, version string }
	samples := map[string][]sample{
		"claude":   {{"2.1.287 (Claude Code)\n", "2.1.287"}},
		"codex":    {{"codex-cli 0.153.4\n", "0.153.4"}},
		"grok":     {{"grok 1.0.52 (0920c8169e27)\n", "1.0.52"}},
		"pi":       {{"1.4.0\n", "1.4.0"}, {"1.5.0-beta.1\n", "1.5.0"}},
		"gemini":   {{"0.65.0\n", "0.65.0"}, {"0.66.0-preview.2\n", "0.66.0"}},
		"opencode": {{"1.18.18\n", "1.18.18"}},
		"cursor":   {{"2026.10.19-6bbf2c4\n", "2026.10.19"}},
		"copilot":  {{"GitHub Copilot CLI 1.0.103.\nRun 'copilot update' to check for updates.\n", "1.0.103"}},
		"qwen":     {{"0.26.0\n", "0.26.0"}, {"0.27.0-nightly.20261020.abc123\n", "0.27.0"}},
	}
	others := []string{
		"grok: DRY and RAD for regular expressions", "Usage: grok [OPTIONS]", "pi 3.14159", "",
		"copilot version: v1.34.0", // AWS Copilot
		"opencode version v0.0.55", // 舊的 Go 版 opencode
		"Usage: cursor-agent [options]",
	}
	for _, d := range builtins() {
		t.Run(string(d.ID), func(t *testing.T) {
			ss, ok := samples[string(d.ID)]
			if !ok {
				t.Fatal("no recorded version output")
			}
			re := regexp.MustCompile(d.Version.Pattern)
			for _, s := range ss {
				if m := re.FindStringSubmatch(s.match); len(m) < 2 || m[1] != s.version {
					t.Errorf("%q on %q = %q, want %s", d.Version.Pattern, s.match, m, s.version)
				}
			}
			for _, o := range others {
				if re.MatchString(o) {
					t.Errorf("%q matches %q", d.Version.Pattern, o)
				}
			}
		})
	}
}

func TestBuiltinsJoinCatalog(t *testing.T) {
	defs, problems := agent.Catalog(builtins(), nil, nil)
	if len(defs) != len(builtins()) || len(problems) != 0 {
		t.Fatalf("defs %d, problems %v", len(defs), problems)
	}
	for _, d := range defs {
		if d.Name == "" || d.Protocol == "" || d.Detect.Command == "" || len(d.Launch.Args) == 0 || d.Install.DocsURL == "" {
			t.Errorf("%s is incomplete: %+v", d.ID, d)
		}
	}
}

// TestBuiltinIcons 確認每個 builtin 都帶內嵌的產品圖示，且符合
// ACP Registry 的圖示規則，頁面才能依主題上色。
func TestBuiltinIcons(t *testing.T) {
	for _, d := range builtins() {
		t.Run(string(d.ID), func(t *testing.T) {
			data, ok := strings.CutPrefix(d.Icon, "data:image/svg+xml;base64,")
			if !ok {
				t.Fatalf("icon %.40q is not an inline SVG", d.Icon)
			}
			svg, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkIcon(svg); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestCheckIcon 確認 checkIcon 擋下 Registry 不收的圖示。
func TestCheckIcon(t *testing.T) {
	const ok = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none"><path fill="currentColor" d="M0 0h24v24H0z"/></svg>`
	cases := map[string]string{
		"ok":         ok,
		"not svg":    `<img width="16" height="16"/>`,
		"too big":    strings.Replace(ok, `width="16" height="16"`, `width="24" height="24"`, 1),
		"color fill": strings.Replace(ok, `fill="currentColor"`, `fill="#D97757"`, 1),
		"stroke":     strings.Replace(ok, `<path `, `<path stroke="red" `, 1),
		"style attr": strings.Replace(ok, `<path `, `<path style="fill:red" `, 1),
		"style tag":  strings.Replace(ok, `<path `, `<style>path{fill:red}</style><path `, 1),
		"script":     strings.Replace(ok, `<path `, `<script>alert(1)</script><path `, 1),
		"empty":      ``,
	}
	for name, svg := range cases {
		if err := checkIcon([]byte(svg)); (err == nil) != (name == "ok") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// checkIcon 套用 ACP Registry 的圖示檢查（registry 的 CONTRIBUTING.md
// 「Icon Validation」）：16×16 的 SVG，fill 與 stroke 只能是 currentColor、
// none 或 inherit；另外不收 style 與 script，顏色不能從 CSS 帶進來。
func checkIcon(svg []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(svg))
	root := true
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if root {
				return errors.New("no <svg> element")
			}
			return nil
		}
		if err != nil {
			return err
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if root {
			if el.Name.Local != "svg" {
				return fmt.Errorf("root is <%s>, want <svg>", el.Name.Local)
			}
			if w, h := attr(el, "width"), attr(el, "height"); w != "16" || h != "16" {
				return fmt.Errorf("size %q×%q, want 16×16", w, h)
			}
			root = false
		}
		switch el.Name.Local {
		case "style", "script", "foreignObject":
			return fmt.Errorf("<%s> is not allowed", el.Name.Local)
		}
		for _, a := range el.Attr {
			switch a.Name.Local {
			case "fill", "stroke":
				if a.Value != "currentColor" && a.Value != "none" && a.Value != "inherit" {
					return fmt.Errorf("<%s %s=%q>: only currentColor, none or inherit", el.Name.Local, a.Name.Local, a.Value)
				}
			case "style", "class":
				return fmt.Errorf("<%s> has a %s attribute", el.Name.Local, a.Name.Local)
			}
		}
	}
}

func attr(el xml.StartElement, name string) string {
	for _, a := range el.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// TestBuiltinsAreFresh 確認每次取得的 builtin 互不共用切片：
// 改動一份不影響下一份，Args 與 Native 也不共用底層陣列。
func TestBuiltinsAreFresh(t *testing.T) {
	for i, d := range builtins() {
		d.Launch.Args[0] = "changed"
		if len(d.Launch.Native) > 0 && d.Launch.Native[0] == "changed" {
			t.Errorf("%s: Native shares Args' array", d.ID)
		}
		if builtins()[i].Launch.Args[0] == "changed" {
			t.Errorf("%s: Definition shares state between calls", d.ID)
		}
	}
}

// TestLiveDiscovery 印出 discovery 在此機器上找到的結果。
// 設定 ORIELHUB_LIVE_DISCOVERY=1 來執行它。
func TestLiveDiscovery(t *testing.T) {
	if os.Getenv("ORIELHUB_LIVE_DISCOVERY") == "" {
		t.Skip("set ORIELHUB_LIVE_DISCOVERY=1 to probe this machine's agents")
	}
	r := &proc.Runner{}
	defer r.Close()
	d := agent.NewDiscovery(agent.DiscoveryOptions{Runner: r, Inherited: proc.Inherited()})
	defer d.Close()
	d.SetAgents(builtins(), nil)
	d.Start()
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, _ := json.MarshalIndent(d.Installations(), "", "  ")
	t.Logf("%s", out)

	c := &agent.RegistryClient{HTTP: http.DefaultClient, Dir: t.TempDir()}
	idx, err := c.Fetch(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	env, err := d.Env(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runnable, native, local := 0, 0, 0
	for _, l := range agent.ListRegistry(idx, builtins(), nil, agent.FindInstalled(env, "", idx)) {
		if l.Runnable {
			runnable++
		}
		if l.Native != "" {
			native++
			t.Logf("hidden: %s (native %s)", l.Entry.ID, l.Native)
		}
		if l.Installed.Path != "" {
			local++
			t.Logf("installed: %s at %s %s", l.Entry.ID, l.Installed.Path, l.Installed.Version)
		}
	}
	t.Logf("registry %s: %d agents, %d runnable here, %d supported natively, %d installed here", idx.Version, len(idx.Agents), runnable, native, local)
}
