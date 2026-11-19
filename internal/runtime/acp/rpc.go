package acp

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	sdk "github.com/coder/acp-go-sdk"
)

// rpcTrack 記下 OrielHub 送出的請求與 agent 的回應：SDK 因斷線回錯時，
// 用它分辨 agent 其實已經回應了（回應到了就照回應處理），還是真的沒回應。
// SDK 每次 Write 寫一則完整的訊息，所以 tap 一次看一則。
type rpcTrack struct {
	mu      sync.Mutex
	sent    map[string]string          // 方法 → 最近一次請求的 ID
	replies map[string]json.RawMessage // 請求 ID → agent 的回應（只留送出過的）
	onSend  func(method string)        // 請求寫出之後呼叫（Prompt 等它）
}

func newRPCTrack() *rpcTrack {
	return &rpcTrack{sent: map[string]string{}, replies: map[string]json.RawMessage{}}
}

// idKey 是 JSON-RPC ID 的比對鍵：數字與字串各照它的值。
func idKey(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	return fmt.Sprint(v)
}

// tap 包住 agent 的 stdin，記下寫出的請求。
type tap struct {
	w io.Writer
	t *rpcTrack
}

// Write 先記下請求的 ID 再寫出：回應不可能早於請求，所以 route 一定認得它；
// 寫出失敗時舊的回應也已清掉，不會被當成這一次的。寫出成功才呼叫 onSend。
func (p tap) Write(b []byte) (int, error) {
	var head struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	request := json.Unmarshal(b, &head) == nil && head.ID != nil && head.Method != ""
	var onSend func(string)
	if request {
		p.t.mu.Lock()
		delete(p.t.replies, p.t.sent[head.Method]) // 只留每個方法最近一次的回應
		p.t.sent[head.Method] = idKey(head.ID)
		onSend = p.t.onSend
		p.t.mu.Unlock()
	}
	n, err := p.w.Write(b)
	if err == nil && onSend != nil {
		onSend(head.Method)
	}
	return n, err
}

// reply 記下 agent 對 OrielHub 請求的回應（route 在交給 SDK 之前呼叫）。
func (t *rpcTrack) reply(id json.RawMessage, line []byte) {
	k := idKey(id)
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, sent := range t.sent {
		if sent == k {
			t.replies[k] = append(json.RawMessage(nil), line...)
			return
		}
	}
}

// decodePrompt 解讀 route 記下的 session/prompt 回應；讀不懂時 ok 為假。
func decodePrompt(line json.RawMessage) (resp sdk.PromptResponse, err error, ok bool) {
	var m struct {
		Result json.RawMessage   `json:"result"`
		Error  *sdk.RequestError `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil {
		return resp, nil, false
	}
	if m.Error != nil {
		return resp, m.Error, true
	}
	if json.Unmarshal(m.Result, &resp) != nil {
		return resp, nil, false
	}
	return resp, nil, true
}

// replyError 是 agent 對這個方法最近一次請求回的錯誤；沒有回錯時為 nil。
func (t *rpcTrack) replyError(method string) *sdk.RequestError {
	var m struct {
		Error *sdk.RequestError `json:"error"`
	}
	if raw := t.replied(method); raw != nil && json.Unmarshal(raw, &m) == nil {
		return m.Error
	}
	return nil
}

// replied 回傳 agent 對這個方法最近一次請求的回應；沒有回應時為 nil。
func (t *rpcTrack) replied(method string) json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.sent[method]
	if !ok {
		return nil
	}
	return t.replies[id]
}
