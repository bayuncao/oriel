package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// Line 是夾具的一行：agent 寫出的訊息、OrielHub 該送來的訊息、寫到
// stderr 的文字，或結束行程。一行只有一個欄位。
type Line struct {
	Agent  json.RawMessage `json:"agent,omitempty"`
	Client json.RawMessage `json:"client,omitempty"`
	Stderr string          `json:"stderr,omitempty"`
	Exit   *int            `json:"exit,omitempty"`
	// CloseStdout 關掉 stdout、行程繼續在（C26）。
	CloseStdout bool `json:"closeStdout,omitempty"`
	// EchoEnv 把這個環境變數寫到 stderr（NAME=值），看 agent 拿到什麼（C40）。
	EchoEnv string `json:"echoEnv,omitempty"`
	// Raw 原樣寫一行到 stdout（不是 JSON 的行，C09）。
	Raw string `json:"raw,omitempty"`
	// Touch 建立這個檔案：前面的行都已寫出（C25 用它確認 agent 沒被卡住）。
	Touch string `json:"touch,omitempty"`
	// CloseStdin 關掉 stdin、行程繼續在：OrielHub 之後的寫入失敗（m3.md CL40 的重試）。
	CloseStdin bool `json:"closeStdin,omitempty"`
	// Hold 停在這裡不結束，直到行程被結束（配 CloseStdin：寫不進去而行程還在）。
	Hold bool `json:"hold,omitempty"`
}

// 夾具裡的佔位值：錄製時換掉、回放時換回（K04）。
const (
	PlaceCwd     = "<cwd>"
	PlaceHome    = "<home>"
	PlaceSession = "<session>"
	PlaceAny     = "<any>"
	// ReplaySession 是回放時 "<session>" 換成的會話 ID。
	ReplaySession = "replay-session"
)

// ReadFixture 讀一個夾具檔（一行一個 Line，空行與 # 開頭的行略過）。
func ReadFixture(path string) ([]Line, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseFixture(b)
}

// ParseFixture 解析夾具的內容。
func ParseFixture(b []byte) ([]Line, error) {
	var out []Line
	for i, raw := range bytes.Split(b, []byte("\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		var l Line
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out = append(out, l)
	}
	return out, nil
}

// WriteFixture 寫出夾具，一行一個 Line；不跳脫 < 與 >，佔位值讀得出來。
func WriteFixture(path string, lines []Line) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			return err
		}
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// matches 回報 got 是否符合 want：want 的物件鍵 got 都要有且相符（got 多出的
// 鍵不看），陣列逐一相符且一樣長，"<any>" 符合任何值。
func matches(want, got any) bool {
	if s, ok := want.(string); ok && s == PlaceAny {
		return true
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			gv, ok := g[k]
			if !ok || !matches(v, gv) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !matches(w[i], g[i]) {
				return false
			}
		}
		return true
	}
	return fmt.Sprint(want) == fmt.Sprint(got)
}

// keyParams 是錄製時 OrielHub 送出的訊息要留下來核對的參數（K02）；其餘
// （版本號、客戶端資訊）每次可能不同，不核對。
var keyParams = map[string][]string{
	"initialize":     {"protocolVersion", "clientCapabilities"},
	"session/new":    {"cwd", "mcpServers", "_meta"},
	"session/load":   {"sessionId", "cwd", "_meta"},
	"session/resume": {"sessionId", "cwd", "_meta"},
	"session/prompt": {"sessionId", "prompt"},
	"session/cancel": {"sessionId"},
}

// Scrub 把錄下的訊息改成夾具：機器相關的值換成佔位值，拿掉 agent 寫的
// _meta（各 agent 私有，內含主機名、實例 ID、模型清單），OrielHub 送出的訊息
// 只留要核對的欄位（含 OrielHub 自己帶的 _meta）。replace 依序把每個鍵換成值（工作目錄、家目錄、會話 ID）。
func Scrub(lines []Line, replace [][2]string) ([]Line, error) {
	out := make([]Line, 0, len(lines))
	for _, l := range lines {
		switch {
		case l.Agent != nil:
			var head struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(l.Agent, &head) == nil && strings.HasPrefix(head.Method, "_") {
				continue // agent 私有的擴充通知：OrielHub 不讀，內含使用者的設定
			}
			v, err := scrubValue(l.Agent, replace, true)
			if err != nil {
				return nil, err
			}
			l.Agent = v
		case l.Client != nil:
			v, err := scrubValue(l.Client, replace, false)
			if err != nil {
				return nil, err
			}
			var m map[string]any
			if err := json.Unmarshal(v, &m); err != nil {
				return nil, err
			}
			keep := map[string]any{}
			for _, k := range []string{"id", "method", "result", "error"} {
				if x, ok := m[k]; ok {
					keep[k] = x
				}
			}
			if p, ok := m["params"].(map[string]any); ok {
				if keys, ok := keyParams[fmt.Sprint(m["method"])]; ok {
					kp := map[string]any{}
					for _, k := range keys {
						if x, ok := p[k]; ok {
							kp[k] = x
						}
					}
					keep["params"] = kp
				}
			}
			b, err := marshal(keep)
			if err != nil {
				return nil, err
			}
			l.Client = b
		}
		out = append(out, l)
	}
	return out, nil
}

func scrubValue(raw json.RawMessage, replace [][2]string, dropMeta bool) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return marshal(scrubAny(v, replace, dropMeta))
}

// marshal 是不跳脫 < 與 > 的 json.Marshal。
func marshal(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// private 是 agent 回應裡 OrielHub 不讀、又帶著使用者設定的欄位（指令與
// 技能清單、模型清單、設定選項）：錄製時清空。
var private = map[string]any{"availableCommands": []any{}, "models": nil, "configOptions": nil, "modes": nil}

// scrubAny 把字串裡的鍵換成值；dropMeta 時去掉每個 _meta 與 private 欄位。
func scrubAny(v any, replace [][2]string, dropMeta bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if dropMeta && k == "_meta" {
				continue
			}
			if p, ok := private[k]; ok && dropMeta {
				if p != nil {
					out[k] = p
				}
				continue
			}
			out[k] = scrubAny(x[k], replace, dropMeta)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = scrubAny(x[i], replace, dropMeta)
		}
		return out
	case string:
		for _, r := range replace {
			if r[0] != "" {
				x = strings.ReplaceAll(x, r[0], r[1])
			}
		}
		return x
	}
	return v
}

// substitute 把回放的訊息裡的佔位值換回這次的值。
func substitute(raw json.RawMessage, cwd string) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return scrubAny(v, [][2]string{{PlaceCwd, cwd}, {PlaceSession, ReplaySession}}, false), nil
}
