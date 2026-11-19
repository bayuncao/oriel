package usage

import (
	"time"

	"github.com/bayuncao/oriel/internal/task"
)

// Category 是 token 的一類。
type Category string

const (
	// Input 是輸入，不含緩存。
	Input Category = "input"
	// CacheRead 是緩存讀取。
	CacheRead Category = "cacheRead"
	// CacheWrite 是緩存寫入。
	CacheWrite Category = "cacheWrite"
	// Output 是輸出，含推理與思考。
	Output Category = "output"
)

// Tokens 是一條用量的四類 token；總數是四類之和。
type Tokens struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Output     int64 `json:"output"`
}

// Total 是四類之和。
func (t Tokens) Total() int64 { return t.Input + t.CacheRead + t.CacheWrite + t.Output }

// Valid 回報四類都不是負數：換算出負數表示 agent 改了欄位的意思。
func (t Tokens) Valid() bool {
	return t.Input >= 0 && t.CacheRead >= 0 && t.CacheWrite >= 0 && t.Output >= 0
}

// Add 回傳逐類相加。
func (t Tokens) Add(o Tokens) Tokens {
	return Tokens{t.Input + o.Input, t.CacheRead + o.CacheRead, t.CacheWrite + o.CacheWrite, t.Output + o.Output}
}

// Max 回傳逐類取較大值：同一條用量的幾份記錄依此合併。
func (t Tokens) Max(o Tokens) Tokens {
	return Tokens{max(t.Input, o.Input), max(t.CacheRead, o.CacheRead), max(t.CacheWrite, o.CacheWrite), max(t.Output, o.Output)}
}

// Record 是一條用量：通常是一次模型呼叫；agent 只按更粗的單位記錄時是一輪
// （Grok）或一次會話結束（Copilot CLI）。
type Record struct {
	Agent task.AgentID
	// Key 在該 agent 內唯一；同一條用量的幾份記錄 Key 相同，合併成一條。
	Key string
	// At 是呼叫的時間，或那一輪完成、那次會話結束的時間。
	At time.Time
	// Session 是原生會話 ID；Parent 是子 agent、子線程、子會話所屬的父會話。
	Session string
	Parent  string
	Model   string
	// Dir 是工作目錄：解析符號連結後的真實路徑，解析不了時按記錄裡的原樣；不知道時為空。
	Dir    string
	Tokens Tokens
}

// Merge 把同一 Key 的另一份記錄併進來：每類取較大值，時間取較早的，會話、
// 父會話、模型、目錄保留先讀到的（先讀到的是空的才用後來的）。
func (r Record) Merge(o Record) Record {
	r.Tokens = r.Tokens.Max(o.Tokens)
	if r.At.IsZero() || (!o.At.IsZero() && o.At.Before(r.At)) {
		r.At = o.At
	}
	r.Session = firstNonEmpty(r.Session, o.Session)
	r.Parent = firstNonEmpty(r.Parent, o.Parent)
	r.Model = firstNonEmpty(r.Model, o.Model)
	r.Dir = firstNonEmpty(r.Dir, o.Dir)
	return r
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Quota 是一個額度桶最新的快照；每個桶（agent 加 Bucket）只保留最新的一份。
type Quota struct {
	Agent  task.AgentID `json:"agent"`
	Bucket string       `json:"bucket"`
	// Name 是 agent 給這個桶的名稱，沒有時為空。
	Name string `json:"name,omitzero"`
	// Plan 是套餐，如 Codex 的 pro。
	Plan    string  `json:"plan,omitzero"`
	Windows []Limit `json:"windows"`
	// At 是寫下這份快照的時間。
	At time.Time `json:"at"`
}

// Limit 是額度的一個窗口（不叫 Window：會遮住 DOM 的同名型別）。
type Limit struct {
	// Minutes 是窗口長度，如 300 是 5 小時，10080 是 7 天。
	Minutes int `json:"minutes"`
	// UsedPercent 是寫下快照時已用的比例（0–100）。
	UsedPercent float64 `json:"usedPercent"`
	// ResetsAt 是下一次重置的時間；不知道時為零值。
	ResetsAt time.Time `json:"resetsAt,omitzero"`
}
