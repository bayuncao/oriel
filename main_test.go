package main

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestMain：以 ORIELHUB_TEST_MAIN=1 重啟自己時照常跑 main，環境由呼叫方給。
func TestMain(m *testing.M) {
	if os.Getenv("ORIELHUB_TEST_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// mygo generate 把 app 編成臨時程式（名字帶進程號）、設 MYGO_GENERATE 執行，
// main 跑到 App.Run 為止（docs/architecture/README.md 的 mygo 約束）：寫出
// TypeScript 客戶端，家目錄下什麼都不建。
func TestGenerateCreatesNothingInHome(t *testing.T) {
	home := t.TempDir()
	out := filepath.Join(t.TempDir(), "mygo.ts")
	// App.Run 不再於 MYGO_GENERATE 下直接返回時，子進程會照常啟動 app 而不結束。
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), "ORIELHUB_TEST_MAIN=1", "MYGO_GENERATE="+out,
		// 資料、快取與日誌目錄都在這些變數下：Linux 先看 XDG_*，Windows 看後三個。
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"USERPROFILE="+home,
		"AppData="+filepath.Join(home, "AppData", "Roaming"),
		"LocalAppData="+filepath.Join(home, "AppData", "Local"),
		// 沒有 go 指令，產生器就不跑 go list：換了家目錄，go 會在這裡寫遙測與建置
		// 快取、重下工具鏈與模組。代價是客戶端少了各套件源碼裡的型別說明與列舉
		// （列舉退成 string），所以只檢查有寫出，不與 src/mygo.ts 比。
		"PATH="+t.TempDir())
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("main under MYGO_GENERATE: %v\n%s", err, b)
	}
	if b, err := os.ReadFile(out); err != nil || len(b) == 0 {
		t.Fatalf("client: %d bytes, %v", len(b), err)
	}
	var made []string
	err := filepath.WalkDir(home, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != home {
			made = append(made, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(made) > 0 {
		t.Fatalf("main made %q before App.Run", made)
	}
}
