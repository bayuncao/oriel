package agent

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/bayuncao/oriel/internal/proc"
)

// LocalInstall 是本機已裝、對應某個 Registry 條目的程式。找到它
// 只表示條目可以直接加入；加入後仍按釘選的條目啟動。
type LocalInstall struct {
	Path    string `json:"path"`
	Version string `json:"version,omitzero"` // npm 套件 package.json 的版本；binary 條目為空
}

// FindInstalled 在 env 的 PATH 與使用者工具目錄中找出本機已裝的
// Registry 條目，不執行任何程式（ADR-25）：npx 條目是解析符號
// 連結後位於其 npm 套件內的命令，binary 條目是與其 cmd 同名的
// 命令。uvx 條目不找：uv tool 的命令名與套件名沒有固定關係。
func FindInstalled(env proc.Env, root string, idx RegistryIndex) map[string]LocalInstall {
	pkgs := map[string][]string{}  // npm 套件名 → 條目 ID
	names := map[string][]string{} // 命令名 → 條目 ID
	for _, e := range idx.Agents {
		c, err := e.Distribution.choose()
		switch {
		case err != nil || c.kind() == distUVX:
		case c.kind() == distNPX:
			p, _ := npx.split(c.pkg.Package)
			pkgs[p] = append(pkgs[p], e.ID)
		case c.kind() == distBinary:
			n := filepath.Base(c.bin.Cmd)
			names[n] = append(names[n], e.ID)
		}
	}

	out := map[string]LocalInstall{}
	found := func(ids []string, li LocalInstall) {
		for _, id := range ids {
			if _, ok := out[id]; !ok {
				out[id] = li
			}
		}
	}
	dirs := proc.SearchDirs(env, root)
	for name, ids := range names {
		if paths := proc.Look(name, dirs); len(paths) > 0 {
			found(ids, LocalInstall{Path: paths[0]})
		}
	}
	if len(pkgs) == 0 {
		return out
	}
	// npm、bun 與 Homebrew 放的 node 命令都是指向套件內的符號連結。
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, de := range entries {
			if de.Type()&fs.ModeSymlink == 0 {
				continue
			}
			path := filepath.Join(dir, de.Name())
			real, err := filepath.EvalSymlinks(path)
			if err != nil || !proc.Executable(path) {
				continue
			}
			for pkg, ids := range pkgs {
				if v, ok := npmVersion(real, []string{pkg}); ok {
					found(ids, LocalInstall{Path: path, Version: v})
				}
			}
		}
	}
	return out
}
