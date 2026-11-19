package hub

import "github.com/bayuncao/oriel/internal/task"

// Update 是訂閱流的一項：恰好一個非空。
type Update struct {
	Event *task.Event `json:"event,omitzero"`
	Delta *Delta      `json:"delta,omitzero"`
}

// Delta 是尚未落庫的文字增量（D60）。
// 客戶端規則：每個 Session 維護 tail = Reset ? Text : tail+Text；
// 頁面顯示 已落庫訊息 + tail。
type Delta struct {
	Session task.SessionID   `json:"session"`
	Channel task.ChannelKind `json:"channel"`
	Text    string           `json:"text"`
	Reset   bool             `json:"reset,omitzero"` // 真：以 Text 取代該 Session 的未落庫尾巴
}
