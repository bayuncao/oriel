package agent

import (
	"cmp"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Problem 是給使用者看的原因或錯誤：穩定的代碼加上參數。前端依
// 代碼翻譯、以參數填空；Error 回傳的英文只供日誌。規則見
// docs/architecture/README.md（「錯誤與提示碼」）。
//
// Installation、Candidate 與 CatalogError 以值保存它；作為 error
// 回傳時一律用 *Problem。errors.Is 依代碼比對，因此帶參數的
// Problem 仍等於同代碼的哨兵（例如 ErrNotRunnable）。
type Problem struct {
	Code   ProblemCode       `json:"code"`
	Params map[string]string `json:"params,omitzero"`
	cause  error             // 只供日誌與 errors.Unwrap
}

// ProblemCode 是 "agent." 開頭的穩定代碼；意義一經發佈不再改變。
type ProblemCode string

const (
	// 候選被拒絕、Installation 損壞或過時的原因。
	CodeNotExecutable         ProblemCode = "agent.not_executable"          // path
	CodeUnreadable            ProblemCode = "agent.unreadable"              // path, detail
	CodeOtherProgram          ProblemCode = "agent.other_program"           // command, brew
	CodeNotFromNPM            ProblemCode = "agent.not_from_npm"            // packages
	CodeInvalidVersionPattern ProblemCode = "agent.invalid_version_pattern" // detail
	CodeVersionTimeout        ProblemCode = "agent.version_timeout"         //
	CodeVersionFailed         ProblemCode = "agent.version_failed"          // detail, output
	CodeVersionMismatch       ProblemCode = "agent.version_mismatch"        // output
	CodeVersionExcluded       ProblemCode = "agent.version_excluded"        // version
	CodeVersionTooOld         ProblemCode = "agent.version_too_old"         // version, min
	CodeDuplicateAgent        ProblemCode = "agent.duplicate_agent"         //
	CodeInvalidSlug           ProblemCode = "agent.invalid_slug"            // slug
	CodeNoCommand             ProblemCode = "agent.no_command"              // slug
	CodeRelativeCommand       ProblemCode = "agent.relative_command"        // slug, command
	CodeRegistryEntryInvalid  ProblemCode = "agent.registry_entry_invalid"  // id, detail
	CodeRegistryEntryMismatch ProblemCode = "agent.registry_entry_mismatch" // id, holds
	CodeNotRunnable           ProblemCode = "agent.not_runnable"            // platform
	CodeUnknownAgent          ProblemCode = "agent.unknown_agent"           //
	CodeNotInstalled          ProblemCode = "agent.not_installed"           //
	CodeClosed                ProblemCode = "agent.closed"                  //
	CodeRegistryNoClient      ProblemCode = "agent.registry_no_client"      //
	CodeRegistryUnreachable   ProblemCode = "agent.registry_unreachable"    // detail
	CodeRegistryStatus        ProblemCode = "agent.registry_status"         // status
	CodeRegistryInvalid       ProblemCode = "agent.registry_invalid"        // detail
	CodeRegistryCacheWrite    ProblemCode = "agent.registry_cache_write"    // detail
	CodeInternal              ProblemCode = "agent.internal"                // detail（不應發生的錯誤）
)

// problemText 是各代碼的英文說明，{name} 由同名參數填入；只供日誌。
// 每個代碼都必須在這裡（TestProblemCodes），它的佔位符就是代碼的參數。
var problemText = map[ProblemCode]string{
	CodeNotExecutable:         "{path} is not an executable file",
	CodeUnreadable:            "cannot read {path}: {detail}",
	CodeOtherProgram:          "another program named {command}: not from Homebrew {brew}",
	CodeNotFromNPM:            "not from npm {packages}",
	CodeInvalidVersionPattern: "invalid version pattern: {detail}",
	CodeVersionTimeout:        "version probe timed out",
	CodeVersionFailed:         "version probe failed: {detail}: {output}",
	CodeVersionMismatch:       "version output {output} does not match",
	CodeVersionExcluded:       "version {version} is known not to work",
	CodeVersionTooOld:         "version {version} is older than {min}",
	CodeDuplicateAgent:        "duplicate agent ID",
	CodeInvalidSlug:           "custom agent slug {slug}: use lowercase letters, digits and -",
	CodeNoCommand:             "custom agent {slug} has no command",
	CodeRelativeCommand:       "custom agent {slug}: command {command} must be a name or an absolute path",
	CodeRegistryEntryInvalid:  "registry entry {id}: {detail}",
	CodeRegistryEntryMismatch: "registry entry {id} holds {holds}",
	CodeNotRunnable:           "no distribution for {platform}",
	CodeUnknownAgent:          "unknown agent",
	CodeNotInstalled:          "the agent is not installed",
	CodeClosed:                "discovery closed",
	CodeRegistryNoClient:      "registry: no HTTP client",
	CodeRegistryUnreachable:   "registry unreachable: {detail}",
	CodeRegistryStatus:        "registry answered {status}",
	CodeRegistryInvalid:       "registry index is invalid: {detail}",
	CodeRegistryCacheWrite:    "cannot write the registry cache: {detail}",
	CodeInternal:              "internal error: {detail}",
}

// ProblemCodes 是本套件的所有代碼，供前端的翻譯表核對。
func ProblemCodes() []ProblemCode {
	return slices.Sorted(maps.Keys(problemText))
}

// Params 是代碼的參數名稱，依說明中出現的順序；翻譯詞條的佔位符
// 必須與它相同。
func (c ProblemCode) Params() []string {
	var out []string
	for _, m := range placeholderRE.FindAllStringSubmatch(problemText[c], -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

var placeholderRE = regexp.MustCompile(`\{([a-z_]+)\}`)

var (
	// ErrUnknownAgent 回傳給不在目錄中的 Agent。
	ErrUnknownAgent = &Problem{Code: CodeUnknownAgent}
	// ErrClosed 在 Close 之後回傳。
	ErrClosed = &Problem{Code: CodeClosed}
	// ErrNotRunnable 表示 Registry 條目在此平台上沒有 OrielHub 可用的
	// 發行方式。
	ErrNotRunnable = &Problem{Code: CodeNotRunnable}
	// ErrCacheWrite 表示 Registry 索引已取得、但寫入快取失敗；Fetch
	// 此時同時回傳可用的索引，下次會重新下載。
	ErrCacheWrite = &Problem{Code: CodeRegistryCacheWrite}
)

// problem 以代碼與成對的參數（名稱、值……）建立 Problem。
func problem(code ProblemCode, params ...string) Problem {
	p := Problem{Code: code}
	if len(params) > 0 {
		p.Params = make(map[string]string, len(params)/2)
		for i := 0; i+1 < len(params); i += 2 {
			p.Params[params[i]] = params[i+1]
		}
	}
	return p
}

// fail 是作為 error 回傳的 Problem，cause 只供日誌。
func fail(code ProblemCode, cause error, params ...string) error {
	p := problem(code, params...)
	p.cause = cause
	return &p
}

// asProblem 取出 err 鏈中的 Problem；err 不是 Problem 時（不應發生）
// 為 CodeInternal 並以 detail 帶上原文，前端不必認得外來的錯誤。
func asProblem(err error) Problem {
	if p, ok := errors.AsType[*Problem](err); ok {
		return *p
	}
	return problem(CodeInternal, "detail", err.Error())
}

// ErrorCode 與 ErrorParams 是邊界（transport）辨認錯誤所用的介面。
func (p *Problem) ErrorCode() string              { return string(p.Code) }
func (p *Problem) ErrorParams() map[string]string { return p.Params }

// Error 是填入參數的英文說明，只供日誌。
func (p *Problem) Error() string {
	text := cmp.Or(problemText[p.Code], string(p.Code))
	for k, v := range p.Params {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	if p.cause != nil {
		text += ": " + p.cause.Error()
	}
	return "agent: " + text
}

func (p *Problem) Unwrap() error { return p.cause }

// Is 讓同代碼的 Problem 彼此相等，不論參數。
func (p *Problem) Is(target error) bool {
	t, ok := target.(*Problem)
	return ok && t.Code == p.Code
}
