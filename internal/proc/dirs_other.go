//go:build !darwin

package proc

// 各平台預設值，相對路徑相對於家目錄。
const (
	defaultShell = "/bin/sh"
	fnmDir       = ".local/share/fnm"
	pnpmDir      = ".local/share/pnpm"
)

// nuConfigDirs 是 nushell 在 $XDG_CONFIG_HOME/nushell 之外
// 存放設定的位置。
var nuConfigDirs []string
