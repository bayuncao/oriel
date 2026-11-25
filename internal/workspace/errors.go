package workspace

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Error 是 workspace 給使用者看的錯誤：穩定的代碼加上參數，前端依代碼
// 翻譯；Error 回傳的英文只供日誌（docs/architecture/README.md 規則 9）。
// errors.Is 依代碼比對，errors.Unwrap 得到原因。
type Error struct {
	Code   Code              `json:"code"`
	Params map[string]string `json:"params,omitzero"`
	cause  error
}

// Code 是 "workspace." 開頭的穩定代碼；意義一經發佈不再改變。
type Code string

const (
	CodeUnsupported    Code = "workspace.unsupported"      // op；不再產生，日誌裡掛起的 Effect 可能帶著它，介面仍要能翻譯
	CodeDirMissing     Code = "workspace.dir_missing"      // path
	CodeGitMissing     Code = "workspace.git_missing"      //
	CodeGitFailed      Code = "workspace.git_failed"       // detail
	CodeGitTooOld      Code = "workspace.git_too_old"      // version want
	CodePathTaken      Code = "workspace.path_taken"       // path
	CodeNoIdentity     Code = "workspace.no_identity"      // path
	CodeDirtyTarget    Code = "workspace.dirty_target"     // path files
	CodeNoRemote       Code = "workspace.no_remote"        // path
	CodeGhMissing      Code = "workspace.gh_missing"       //
	CodeGhFailed       Code = "workspace.gh_failed"        // detail
	CodeBranchMoved    Code = "workspace.branch_moved"     // branch
	CodeNoBaseOnRemote Code = "workspace.no_remote_branch" // branch
)

// codeText 是各代碼的英文說明，{name} 由同名參數填入；只供日誌。
// 它的佔位符就是代碼的參數（Code.Params）。
var codeText = map[Code]string{
	CodeUnsupported:    "{op} is not supported by this version",
	CodeDirMissing:     "{path} is not a directory",
	CodeGitMissing:     "no usable git found",
	CodeGitFailed:      "git failed: {detail}",
	CodeGitTooOld:      "git {version} is too old, {want} or newer is needed",
	CodePathTaken:      "{path} already exists and is not this worktree",
	CodeNoIdentity:     "git has no user.name or user.email to commit with in {path}",
	CodeDirtyTarget:    "uncommitted changes in {path} would be overwritten: {files}",
	CodeNoRemote:       "{path} has no origin remote to push to",
	CodeGhMissing:      "no usable gh found",
	CodeGhFailed:       "gh failed: {detail}",
	CodeBranchMoved:    "{branch} kept moving while merging",
	CodeNoBaseOnRemote: "{branch} is not on origin, so a pull request cannot target it",
}

// Codes 是本套件的所有代碼。
func Codes() []Code { return slices.Sorted(maps.Keys(codeText)) }

// Params 是代碼的參數名稱，依說明中出現的順序。
func (c Code) Params() []string { return placeholders(codeText[c]) }

func fail(code Code, cause error, params ...string) *Error {
	e := &Error{Code: code, cause: cause}
	if len(params) > 0 {
		e.Params = make(map[string]string, len(params)/2)
		for i := 0; i+1 < len(params); i += 2 {
			e.Params[params[i]] = params[i+1]
		}
	}
	return e
}

func (e *Error) ErrorCode() string              { return string(e.Code) }
func (e *Error) ErrorParams() map[string]string { return e.Params }

func (e *Error) Error() string {
	text := cmp.Or(codeText[e.Code], string(e.Code))
	for k, v := range e.Params {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return "workspace: " + text
}

func (e *Error) Unwrap() error { return e.cause }

// Is 讓同代碼的 Error 彼此相等，不論參數。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

var placeholderRE = regexp.MustCompile(`\{([a-z_]+)\}`)

func placeholders(text string) []string {
	var out []string
	for _, m := range placeholderRE.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}
