package mygoipc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/transport"
)

// PV03、PV05：隱私頁的內容：各類請求的去處、目錄與大小；接上之前回 not_ready。
func TestPrivacy(t *testing.T) {
	var we *transport.Error
	if _, err := (&Desktop{}).Privacy(context.Background()); !errors.As(err, &we) || we.Code != string(CodeNotReady) {
		t.Fatalf("before connect: %v", err)
	}
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "a"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "sub", "b"), make([]byte, 23), 0o644); err != nil {
		t.Fatal(err)
	}
	// 任務的工作樹在資料目錄裡，不算進大小。
	worktrees := filepath.Join(data, "worktrees")
	if err := os.MkdirAll(filepath.Join(worktrees, "w-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktrees, "w-1", "big"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Desktop{}
	ConnectDesktop(s, DesktopParts{Remote: true, UpdateSource: UpdateSource{Repo: "bayuncao/oriel"},
		Folders:   map[PrivacyFolder]string{FolderData: data, FolderLogs: filepath.Join(data, "missing")},
		Worktrees: worktrees})
	p, err := s.Privacy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Data.Path != data || p.Data.Size != 123 || p.Data.Excludes != worktrees {
		t.Errorf("data %+v, want 123 bytes without %s", p.Data, worktrees)
	}
	if p.Logs.Size != -1 || p.Logs.Excludes != "" {
		t.Errorf("missing logs %+v, want size -1", p.Logs)
	}
	// 沒有接上能自我更新的 updater：這個構建不檢查更新，也不寫去處。
	if p.Updates || p.UpdateSource.Repo != "" {
		t.Errorf("updates %v %+v", p.Updates, p.UpdateSource)
	}
	if p.Registry != agent.RegistryURL || !p.Remote || len(p.StatusPages) == 0 {
		t.Errorf("registry %q remote %v pages %d", p.Registry, p.Remote, len(p.StatusPages))
	}
	for i := 1; i < len(p.StatusPages); i++ {
		if p.StatusPages[i-1].Name > p.StatusPages[i].Name {
			t.Fatalf("status pages not sorted: %v", p.StatusPages)
		}
	}
}

// PV05：在訪達裡打開資料或日誌目錄；頁面只給名稱，別的名稱回 unknown_folder。
func TestRevealFolder(t *testing.T) {
	var opened []string
	s := &Desktop{openPath: func(p string) error { opened = append(opened, p); return nil }}
	var we *transport.Error
	if err := s.RevealFolder(FolderData); !errors.As(err, &we) || we.Code != string(CodeNotReady) {
		t.Fatalf("before connect: %v", err)
	}
	ConnectDesktop(s, DesktopParts{Folders: map[PrivacyFolder]string{FolderData: "/d", FolderLogs: "/l"}})
	if err := s.RevealFolder(FolderLogs); err != nil {
		t.Fatal(err)
	}
	if err := s.RevealFolder("/etc"); !errors.As(err, &we) || we.Code != string(CodeUnknownFolder) {
		t.Fatalf("unknown: %v", err)
	}
	if len(opened) != 1 || opened[0] != "/l" {
		t.Fatalf("opened %v", opened)
	}
}
