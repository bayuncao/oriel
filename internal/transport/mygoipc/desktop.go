package mygoipc

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/i18n"

	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/updater"
)

// LanguageSetting 是用戶選單選的介面語言（實作是 app.Language，D73）。
type LanguageSetting interface {
	// Choice 回傳目前的選擇，"" 是跟隨系統。
	Choice() string
	// Set 記住 tag（"" 跟隨系統）並讓 app 立刻改用它。
	Set(tag string) error
}

// KeepAwakeSetting 是“Agent 運行時防止電腦睡眠”的開關（實作是 app.KeepAwake，D80）。
type KeepAwakeSetting interface {
	// On 回答開關是否開著。
	On() bool
	// Set 記住開關並立刻生效。
	Set(on bool) error
}

// ThemeSetting 是介面的外觀（實作是 app.Theme，D158）。
type ThemeSetting interface {
	// Choice 回傳目前的選擇："" 跟隨系統，"light" 或 "dark"。
	Choice() string
	// Set 讓 app 立刻改用它並記住；寫不進去時這次照樣生效並回錯誤。
	Set(choice string) error
}

// Languages 是用戶選單語言切換的內容。
type Languages struct {
	// Choice 是目前的選擇，"" 是跟隨系統。
	Choice string `json:"choice"`
	// Shown 是 app 正在顯示的語言（跟隨系統時由系統語言決定）。
	Shown string `json:"shown"`
	// Options 是 app 寫成的語言，源語言在前。
	Options []LanguageOption `json:"options"`
}

// LanguageOption 是一個可選的語言。
type LanguageOption struct {
	Tag string `json:"tag"`
	// Name 是語言以自己寫出的名字，如 "简体中文"。
	Name string `json:"name"`
}

// Desktop 是與後端無關的桌面能力（BindAs "Desktop"）。main 在 App.WhenReady
// 裡、後端與第一個視窗之前接上，所以後端就緒前也能用；接上之前
// SetLanguage、WatchUpdates、CheckForUpdates 與 WatchReleaseNotes 回 mygoipc.not_ready。
type Desktop struct {
	mu    sync.Mutex
	parts DesktopParts
	ready bool // ConnectDesktop 之後為真
	// recent 是搜尋面板的最近搜尋（RecentSearches），新的在前。
	recent []string
	// onMain 在主線程上執行 fn 並等它返回；nil 是 mygo.RunOnMain（事件迴圈還沒
	// 開始時一直等，已經結束時不執行就返回）。測試的接縫，建構時設定。
	onMain func(fn func())
	// openTask 廣播 open:task；nil 是 OpenTask.Broadcast。測試的接縫，建構時設定。
	openTask func(OpenTaskEvent)
	// openExternal 以系統預設的應用打開連結；nil 是 mygo.Shell.OpenExternal。測試的接縫，建構時設定。
	openExternal func(link string) error
	// openPath 在訪達裡打開目錄；nil 是 mygo.Shell.OpenPath。測試的接縫，建構時設定。
	openPath func(path string) error
}

// maxRecentSearches 是搜尋面板記住的最近搜尋條數（D67）。
const maxRecentSearches = 3

// DesktopParts 是 ConnectDesktop 接上的部分，沒接上的是 nil。
type DesktopParts struct {
	// Show 打開或聚焦主視窗，只在主線程上執行。
	Show func()
	// Lang 是用戶選單選的介面語言。
	Lang LanguageSetting
	// Updates 是自動更新。
	Updates *updater.Updater
	// Notes 是更新後第一次啟動的更新說明。
	Notes *updater.Notes
	// KeepAwake 是 Agent 運行時防止電腦睡眠的開關。
	KeepAwake KeepAwakeSetting
	// Theme 是介面的外觀。
	Theme ThemeSetting
	// Remote 為真時這個構建有遠程；正式版先沒有（D149）。
	Remote bool
	// UpdateSource 是這個構建取更新的地方（隱私頁，PV03）；本機構建是空的。
	UpdateSource UpdateSource
	// Folders 是隱私頁的資料與日誌目錄（PV05）；接上時一定有。
	Folders map[PrivacyFolder]string
	// Worktrees 是資料目錄裡任務的工作樹，資料目錄的大小不算它（PV05）。
	Worktrees string
}

// ConnectDesktop 給 Desktop 接上 p（D61：包級函式，不是頁面可呼叫的方法）。
func ConnectDesktop(s *Desktop, p DesktopParts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parts, s.ready = p, true
}

// connected 回傳目前接上的部分。
func (s *Desktop) connected() DesktopParts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parts
}

// ChooseFolder 以附在呼叫頁視窗上的系統面板選一個資料夾，
// 回傳解開符號連結後的絕對路徑（同 hub 登記的路徑，D72）；取消回傳 ""。
func (*Desktop) ChooseFolder(ctx context.Context) (string, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent: mygo.CallerWindow(ctx), Directory: true,
		CreateDirectories: true})
	if err != nil {
		return "", wire(slog.Default(), "ChooseFolder", err)
	}
	if len(paths) == 0 {
		return "", nil
	}
	if real := realFolder(paths[0]); real != "" {
		return real, nil
	}
	return paths[0], nil // 剛被刪掉：照原樣交給頁面，建 Task 時由 hub 說明
}

// Folder 在 path 是一個存在的資料夾（指向資料夾的符號連結也算）時回傳解開
// 符號連結後的路徑，否則回傳 ""。拖放只給路徑，分不出檔案與資料夾，Task 舞台
// 以它判斷拖進來的能不能用（m1.md T18）；回傳的是真實路徑，與 hub 登記的
// Workspace 路徑（D72）比對得上（m2.md T33）。相對路徑一律是 ""：拖放給的都是
// 絕對路徑，相對路徑會依行程的目錄解讀。
func (*Desktop) Folder(path string) string {
	return realFolder(path)
}

// realFolder 是 Desktop.Folder 的實作，ChooseFolder 也用它。
func realFolder(path string) string {
	if !filepath.IsAbs(path) {
		return ""
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return ""
	}
	return real
}

// linkSchemes 是 OpenLink 肯打開的協議（m2.md V10）：網頁與郵件。file:、javascript: 與 app 自己的協議一律不開。
var linkSchemes = []string{"http", "https", "mailto"}

// OpenLink 以系統預設的瀏覽器（mailto 是郵件應用）打開對話裡的連結，應用的視窗不跳走（m2.md V10）。
// 只開 http、https 與 mailto，其他回 mygoipc.unsupported_link。
func (s *Desktop) OpenLink(link string) error {
	u, err := url.Parse(link)
	if err != nil || !slices.Contains(linkSchemes, strings.ToLower(u.Scheme)) ||
		(u.Scheme != "mailto" && u.Host == "") {
		return wire(slog.Default(), "OpenLink", errUnsupportedLink)
	}
	open := s.openExternal
	if open == nil {
		open = mygo.Shell.OpenExternal
	}
	if err := open(u.String()); err != nil {
		return wire(slog.Default(), "OpenLink", err)
	}
	return nil
}

// AttachRejection 是一個路徑不能作為消息附件的原因（m2.md V45、V46）。
type AttachRejection string

const (
	// AttachOutside：不在 Task 的工作目錄裡（解開符號連結後比對）。
	AttachOutside AttachRejection = "outside"
	// AttachNotFile：是資料夾、不存在或不是一般檔案。
	AttachNotFile AttachRejection = "not_file"
)

// Attachable 是一個路徑能不能作為消息附件（D100：只有工作目錄裡的檔案路徑）。
type Attachable struct {
	// Path 是解開符號連結後的絕對路徑；不能附時是空的。
	Path string `json:"path,omitzero"`
	// Rejection 是不能附的原因；能附時是空的。
	Rejection AttachRejection `json:"rejection,omitzero"`
}

// Attachable 判斷 path 能不能附在 dir（Task 的工作目錄）裡發出的消息上：兩者解開符號連結後，path 是 dir
// 之下的一般檔案。拖放與選檔都用它（m2.md V44–V46）。相對路徑一律不能附：拖放給的都是絕對路徑。
func (*Desktop) Attachable(dir, path string) Attachable {
	return attachable(dir, path)
}

func attachable(dir, path string) Attachable {
	if !filepath.IsAbs(path) {
		return Attachable{Rejection: AttachNotFile}
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Attachable{Rejection: AttachNotFile}
	}
	if info, err := os.Stat(real); err != nil || !info.Mode().IsRegular() {
		return Attachable{Rejection: AttachNotFile}
	}
	root := realFolder(dir)
	if root == "" {
		return Attachable{Rejection: AttachOutside}
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Attachable{Rejection: AttachOutside}
	}
	return Attachable{Path: real}
}

// ChooseFiles 以附在呼叫頁視窗上的系統面板選檔案，從 dir（Task 的工作目錄）打開，可多選；每個選中的檔案
// 都經 Attachable 判斷（m2.md V44）。取消回傳空的切片。
func (*Desktop) ChooseFiles(ctx context.Context, dir string) ([]Attachable, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent: mygo.CallerWindow(ctx), DefaultPath: dir, Multiple: true})
	if err != nil {
		return nil, wire(slog.Default(), "ChooseFiles", err)
	}
	out := make([]Attachable, 0, len(paths))
	for _, p := range paths {
		out = append(out, attachable(dir, p))
	}
	return out, nil
}

// ShowMain 恢復並聚焦主視窗，沒有就開一個；從菜單欄面板呼叫時順手關掉面板。
// 做完才返回。
func (s *Desktop) ShowMain(ctx context.Context) {
	show := s.connected().Show
	caller := mygo.CallerWindow(ctx)
	// 綁定方法跑在各自的 goroutine 上，而主視窗只在主線程上開關（main.go）：
	// 兩件事交給主線程依序做，與主線程上同時的打開（如 OnActivate）不交錯。
	s.runOnMain(func() {
		if show != nil {
			show()
		}
		if caller != nil && caller.IsFlyout() {
			caller.Close()
		}
	})
}

// ShowTask 打開主視窗並轉到這個 Task（菜單欄面板點一條“需要處理”，m2.md M17）：
// 先做完 ShowMain，再廣播 open:task，主視窗的頁面收到後導航（同點通知，N10）。
// 頁面還沒載入時，事件在它就緒後送達。
func (s *Desktop) ShowTask(ctx context.Context, id task.TaskID) {
	s.ShowMain(ctx)
	ev := OpenTaskEvent{Task: id}
	if s.openTask != nil {
		s.openTask(ev)
		return
	}
	OpenTask.Broadcast(ev)
}

// runOnMain 在主線程上執行 fn，等它返回。
func (s *Desktop) runOnMain(fn func()) {
	if s.onMain != nil {
		s.onMain(fn)
		return
	}
	mygo.RunOnMain(fn)
}

// Quit 退出應用（有運行中 Session 時的確認隨 M2，D58）。
func (*Desktop) Quit() { mygo.App.Quit() }

// Languages 回傳用戶選單語言切換的內容。
func (s *Desktop) Languages() Languages {
	out := Languages{Shown: mygo.App.Locale()}
	if lang := s.connected().Lang; lang != nil {
		out.Choice = lang.Choice()
	}
	for _, tag := range mygo.App.Languages() {
		out.Options = append(out.Options, LanguageOption{Tag: tag, Name: i18n.Autonym(tag)})
	}
	return out
}

// SetLanguage 記住介面語言並立刻改用它；tag 為 "" 是跟隨系統。各頁面收到
// mygo:locale 後重畫（docs/acceptance/m1.md 語言一節）。
func (s *Desktop) SetLanguage(tag string) error {
	lang := s.connected().Lang
	if lang == nil {
		return wire(slog.Default(), "SetLanguage", errNotReady)
	}
	if err := lang.Set(tag); err != nil {
		return wire(slog.Default(), "SetLanguage", err)
	}
	return nil
}

// Theme 回答介面的外觀："" 跟隨系統，"light" 或 "dark"（PF02）。接上之前回 mygoipc.not_ready。
func (s *Desktop) Theme() (string, error) {
	t := s.connected().Theme
	if t == nil {
		return "", wire(slog.Default(), "Theme", errNotReady)
	}
	return t.Choice(), nil
}

// SetTheme 讓 app 立刻改用外觀並記住它（PF03）：原生視窗、菜單欄面板與頁面的
// prefers-color-scheme 一起換。不認得的值回 app.unknown_theme；寫不進去回
// app.theme_save_failed，這次照樣生效（PF08）。
func (s *Desktop) SetTheme(choice string) error {
	t := s.connected().Theme
	if t == nil {
		return wire(slog.Default(), "SetTheme", errNotReady)
	}
	if err := t.Set(choice); err != nil {
		return wire(slog.Default(), "SetTheme", err)
	}
	return nil
}

// RemoteAvailable 回答這個構建有沒有遠程（D149，DV17）：沒有時頁面不顯示 設置 › 遠程。
// 接上之前回 mygoipc.not_ready，不猜。
func (s *Desktop) RemoteAvailable() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return false, wire(slog.Default(), "RemoteAvailable", errNotReady)
	}
	return s.parts.Remote, nil
}

// KeepAwake 回答“Agent 運行時防止電腦睡眠”是否開著（D80，m2.md KA10）。接上之前回 mygoipc.not_ready，
// 不猜預設值：頁面拿到猜的值再切換，會把使用者關掉的開關寫回開。
func (s *Desktop) KeepAwake() (bool, error) {
	k := s.connected().KeepAwake
	if k == nil {
		return false, wire(slog.Default(), "KeepAwake", errNotReady)
	}
	return k.On(), nil
}

// SetKeepAwake 記住開關並立刻生效（KA07、KA13）；寫不進去回 app.keep_awake_save_failed。
func (s *Desktop) SetKeepAwake(on bool) error {
	k := s.connected().KeepAwake
	if k == nil {
		return wire(slog.Default(), "SetKeepAwake", errNotReady)
	}
	if err := k.Set(on); err != nil {
		return wire(slog.Default(), "SetKeepAwake", err)
	}
	return nil
}

// RecentSearches 回傳搜尋面板的最近搜尋（D67），新的在前。只在記憶體裡：關掉主視窗
// 再打開還在（應用照常運行，D58），退出應用就沒了；不寫進日誌。
func (s *Desktop) RecentSearches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.recent)
}

// SetRecentSearches 換掉搜尋面板的最近搜尋，新的在前；去掉每條首尾的空白與空的，
// 最多留 3 條（maxRecentSearches）。
func (s *Desktop) SetRecentSearches(searches []string) {
	var keep []string
	for _, q := range searches {
		if q = strings.TrimSpace(q); q != "" && len(keep) < maxRecentSearches {
			keep = append(keep, q)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = keep
}

// WatchUpdates 先推目前的更新狀態，之後每次變化推一次（D76）；頁面離開或
// 應用退出時結束。
func (s *Desktop) WatchUpdates(ctx context.Context, states *mygo.Channel[updater.Status]) error {
	updates := s.connected().Updates
	if updates == nil {
		return wire(slog.Default(), "WatchUpdates", errNotReady)
	}
	if err := pageLeft(ctx, updates.Watch(ctx, states.Send)); err != nil {
		return wire(slog.Default(), "WatchUpdates", err)
	}
	return nil
}

// CheckForUpdates 檢查更新，找到新版本就下載安裝；與進行中的檢查併成一次。
// 回傳結束時的狀態：idle 是已是最新，ready 是已裝好；失敗回
// updater.check_failed 或 updater.install_failed。頁面離開只結束等待，
// 檢查照常進行。
func (s *Desktop) CheckForUpdates(ctx context.Context) (updater.Status, error) {
	updates := s.connected().Updates
	if updates == nil {
		return updater.Status{}, wire(slog.Default(), "CheckForUpdates", errNotReady)
	}
	st, err := updates.Check(ctx)
	if err != nil {
		return updater.Status{}, wire(slog.Default(), "CheckForUpdates", err)
	}
	return st, nil
}

// RestartToUpdate 重啟到裝好的新版本，走正常的退出流程；沒有裝好的更新時
// 什麼都不做。
func (s *Desktop) RestartToUpdate() {
	if updates := s.connected().Updates; updates != nil {
		updates.Restart()
	}
}

// WatchReleaseNotes 在更新後第一次啟動、這個版本的說明拉到時推一次（m2.md RN02、RN09）；
// 已顯示過或不必顯示時什麼都不推。頁面離開或應用退出時結束。
func (s *Desktop) WatchReleaseNotes(ctx context.Context, notes *mygo.Channel[updater.ReleaseNotes]) error {
	n := s.connected().Notes
	if n == nil {
		return wire(slog.Default(), "WatchReleaseNotes", errNotReady)
	}
	if err := pageLeft(ctx, n.Watch(ctx, notes.Send)); err != nil {
		return wire(slog.Default(), "WatchReleaseNotes", err)
	}
	return nil
}

// ReleaseNotesShown 記下 version 的說明已經顯示（RN03）：之後不再推，下次啟動也不顯示。
func (s *Desktop) ReleaseNotesShown(version string) {
	if n := s.connected().Notes; n != nil {
		n.Shown(version)
	}
}
