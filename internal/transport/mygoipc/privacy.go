package mygoipc

// privacy.go：設定 › 隱私的內容（docs/acceptance/settings.md 第 3 節，D158）：OrielHub 自己會發出的請求
// 去哪裡，以及本機的資料與日誌目錄。

import (
	"context"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/egoist/mygo"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/updater"
)

// UpdateSource 是這個構建取更新的地方（app 的渠道設定，D151）；本機構建沒有。
type UpdateSource struct {
	// Repo 是發布更新的 GitHub 倉庫（owner/name）。
	Repo string `json:"repo,omitzero"`
	// Mirror 是備用源的地址（正式版是香港的 updates.oriel.today）。
	Mirror string `json:"mirror,omitzero"`
}

// Privacy 是隱私頁的內容（PV03、PV05）。
type Privacy struct {
	// Updates 為假時這個構建不檢查更新（本機構建）。
	Updates bool `json:"updates"`
	// UpdateSource 是取更新的地方；Updates 為假時是空的。
	UpdateSource UpdateSource `json:"updateSource"`
	// StatusPages 是概覽頁開著時讀的各廠商公開狀態頁。
	StatusPages []StatusPage `json:"statusPages"`
	// Registry 是打開 Registry 頁時取的 ACP Registry。
	Registry string `json:"registry"`
	// Remote 為假時這個版本沒有遠程（D149）。
	Remote bool `json:"remote"`
	// Data 與 Logs 是本機的資料與日誌目錄。
	Data Folder `json:"data"`
	Logs Folder `json:"logs"`
}

// StatusPage 是一家廠商的公開狀態頁。
type StatusPage struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Folder 是本機的一個目錄與它的大小。
type Folder struct {
	Path string `json:"path"`
	// Size 是目錄裡一般檔案的位元組數；算不出來（讀不了、目錄不在）時是 -1（PV05）。
	Size int64 `json:"size"`
	// Excludes 是不算進 Size 的子目錄（資料目錄裡任務的工作樹：是倉庫的檢出，不是 OrielHub 的資料，
	// 也大得每次打開頁面都走一遍太慢）；沒有時是空的。
	Excludes string `json:"excludes,omitzero"`
}

// PrivacyFolder 是頁面可以在訪達裡打開的目錄（PV05）：頁面只給名稱，不給路徑。
type PrivacyFolder string

const (
	FolderData PrivacyFolder = "data"
	FolderLogs PrivacyFolder = "logs"
)

// Privacy 回傳隱私頁的內容；目錄大小每次重新算（PV05）。接上之前回 mygoipc.not_ready。
func (s *Desktop) Privacy(ctx context.Context) (Privacy, error) {
	p := s.connected()
	if p.Folders == nil {
		return Privacy{}, wire(slog.Default(), "Privacy", errNotReady)
	}
	out := Privacy{Registry: agent.RegistryURL, Remote: p.Remote, StatusPages: statusPages()}
	if p.Updates != nil && p.Updates.Status().Phase != updater.Unavailable {
		out.Updates, out.UpdateSource = true, p.UpdateSource
	}
	out.Data = folder(ctx, p.Folders[FolderData], p.Worktrees)
	out.Logs = folder(ctx, p.Folders[FolderLogs], "")
	return out, nil
}

// RevealFolder 在訪達裡打開資料或日誌目錄（PV05）；不是這兩個名稱回 mygoipc.unknown_folder。
func (s *Desktop) RevealFolder(name PrivacyFolder) error {
	p := s.connected()
	if p.Folders == nil {
		return wire(slog.Default(), "RevealFolder", errNotReady)
	}
	dir, ok := p.Folders[name]
	if !ok {
		return wire(slog.Default(), "RevealFolder", errUnknownFolder)
	}
	open := s.openPath
	if open == nil {
		open = mygo.Shell.OpenPath
	}
	if err := open(dir); err != nil {
		return wire(slog.Default(), "RevealFolder", err)
	}
	return nil
}

// statusPages 是各廠商的狀態頁，依名稱排序。
func statusPages() []StatusPage {
	var out []StatusPage
	for _, v := range provider.Catalog() {
		out = append(out, StatusPage{Name: v.Name, URL: v.Page})
	}
	slices.SortFunc(out, func(a, b StatusPage) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// folder 算目錄裡一般檔案的總大小；讀不了的子目錄跳過，整個目錄讀不了時是 -1。
// excludes 在 dir 裡時不算它（記在 Excludes），不在 dir 裡時不用。
func folder(ctx context.Context, dir, excludes string) Folder {
	out := Folder{Path: dir, Size: -1}
	if rel, err := filepath.Rel(dir, excludes); excludes == "" || err != nil || rel == "." || !filepath.IsLocal(rel) {
		excludes = ""
	}
	out.Excludes = excludes
	if dir == "" {
		return out
	}
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if path == dir {
				return err
			}
			return nil // 讀不了的子目錄不算
		}
		if d.IsDir() && path == excludes {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if err == nil {
		out.Size = total
	}
	return out
}
