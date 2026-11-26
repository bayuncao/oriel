package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"slices"

	"github.com/bayuncao/oriel/internal/agent"
	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
	"github.com/bayuncao/oriel/internal/transport"
	"github.com/bayuncao/oriel/internal/usage"
)

// 加密通道上的 RPC（D145）：每則 Noise 訊息是一個 JSON 物件。
//
//   - App → Mac：請求 Request{id, method, params}；取消訂閱 Request{id, cancel: true}，
//     id 是要取消的那個訂閱。id 由 App 選，同一條連線上不重複使用進行中的 id。
//   - Mac → App：一般方法回一則 Reply{id, result} 或 Reply{id, error}；訂閱方法先推
//     零到多則 Reply{id, push}，最後以 Reply{id, end: true}（App 取消了）或
//     Reply{id, error} 結束。
//
// 解不開的訊息（Noise 驗證失敗）或不是 JSON 的請求，Mac 斷開這條連線。

// Request 是 App 送給 Mac 的一則訊息。
type Request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method,omitzero"`
	Params json.RawMessage `json:"params,omitzero"`
	Cancel bool            `json:"cancel,omitzero"`
}

// Reply 是 Mac 送給 App 的一則訊息；Result、Push、End、Error 恰有一個。
type Reply struct {
	ID     uint64           `json:"id"`
	Result json.RawMessage  `json:"result,omitzero"`
	Push   json.RawMessage  `json:"push,omitzero"`
	End    bool             `json:"end,omitzero"`
	Error  *transport.Error `json:"error,omitzero"`
}

// TaskParams 是 Snapshot 的參數。
type TaskParams struct {
	Task task.TaskID `json:"task"`
}

// SubscribeParams 是 Subscribe 的參數：From 是 App 已收到的最後一個 Seq，
// 重連後從它續訂（RC02）。
type SubscribeParams struct {
	Task task.TaskID `json:"task"`
	From task.Seq    `json:"from"`
}

// CommandParams 是 Command 的參數。Command 的 Origin 與 Device 由 hub 按
// 呼叫者填入，App 送來的不算（RP16）。
type CommandParams struct {
	Task    task.TaskID  `json:"task"`
	Command task.Command `json:"command"`
}

// CreateTaskParams 是 CreateTask 的參數，同桌面的 Hub.CreateTask：Agent 為空時
// Task 不帶 Session；不為空時由 hub 解析成 Launch Snapshot，App 給不了要
// 運行的程序（RP10、RP15）。
type CreateTaskParams struct {
	Dir   string       `json:"dir"`
	Title string       `json:"title"`
	Brief string       `json:"brief"`
	Agent task.AgentID `json:"agent"`
}

// AgentSource 是 agent 目錄：內置與自訂的定義，接上發現結果。app 的 *Agents
// 滿足它（同 mygoipc.AgentSource 的這兩個方法）。
type AgentSource interface {
	Definitions() []agent.Definition
	Installations() []agent.Installation
}

// call 是一次方法呼叫的環境：以哪台設備的身份，呼叫哪個 hub、agent 目錄與用量。
type call struct {
	hub    *hub.Hub
	agents AgentSource
	usage  *usage.Tracker
	p      hub.Principal
}

// launches 是帶 Launch Snapshot 的命令：手機不能指定要運行的程序（RP15）。
var launches = []task.CommandKind{task.CmdCreateSession, task.CmdFanOut}

// method 是白名單裡的一個方法：unary 回一個結果，watch 推送直到 ctx 結束或出錯。
type method struct {
	unary func(ctx context.Context, c call, params json.RawMessage) (any, error)
	watch func(ctx context.Context, c call, params json.RawMessage, push func(any) error) error
}

// methods 是遠程開放的方法（D145 的白名單，TestMethods 鎖住），名字同桌面的
// 服務：Hub 的方法不帶前綴，其餘帶服務名。不在表裡的回 remote.not_allowed（CN07）。
var methods = map[string]method{
	"Tasks": {unary: func(ctx context.Context, c call, params json.RawMessage) (any, error) {
		if err := decodeParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return c.hub.Tasks(ctx, c.p)
	}},
	"Snapshot": {unary: func(ctx context.Context, c call, params json.RawMessage) (any, error) {
		var p TaskParams
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		return c.hub.Snapshot(ctx, c.p, p.Task)
	}},
	"Attention": {unary: func(ctx context.Context, c call, params json.RawMessage) (any, error) {
		if err := decodeParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return c.hub.Attention(ctx, c.p)
	}},
	"Confirmations": {unary: func(ctx context.Context, c call, params json.RawMessage) (any, error) {
		if err := decodeParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return c.hub.Confirmations(ctx, c.p)
	}},
	"Workspaces": {unary: func(ctx context.Context, c call, params json.RawMessage) (any, error) {
		if err := decodeParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return c.hub.Workspaces(ctx, c.p)
	}},
	"WatchTasks": {watch: func(ctx context.Context, c call, params json.RawMessage, push func(any) error) error {
		if err := decodeParams(params, &struct{}{}); err != nil {
			return err
		}
		return c.hub.WatchTasks(ctx, c.p, func(v hub.TaskView) error { return push(v) })
	}},
	"Subscribe": {watch: func(ctx context.Context, c call, params json.RawMessage, push func(any) error) error {
		var p SubscribeParams
		if err := decodeParams(params, &p); err != nil {
			return err
		}
		return c.hub.Subscribe(ctx, c.p, p.Task, p.From, func(u hub.Update) error { return push(u) })
	}},
	"Command": {unary: func(ctx context.Context, c call, params json.RawMessage) (any, error) {
		var p CommandParams
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		if slices.Contains(launches, p.Command.Kind) {
			return nil, errNotAllowed
		}
		return c.hub.Command(ctx, c.p, p.Task, p.Command)
	}},
	"CreateTask": {unary: func(ctx context.Context, c call, params json.RawMessage) (any, error) {
		var p CreateTaskParams
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		id, _, err := c.hub.CreateTaskWithAgent(ctx, c.p, p.Dir, task.CreateTask{Title: p.Title, Brief: p.Brief}, p.Agent, "") // 預設 Profile（m4.md PO18）
		return id, err
	}},
	"Agents.List": {unary: func(ctx context.Context, c call, params json.RawMessage) (any, error) {
		if err := decodeParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return agent.Rows(c.agents.Definitions(), c.agents.Installations()), nil
	}},
	"Usage.Summary": {unary: func(ctx context.Context, c call, params json.RawMessage) (any, error) {
		if err := decodeParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return c.usage.Summary(ctx)
	}},
	"Usage.Refresh": {unary: func(ctx context.Context, c call, params json.RawMessage) (any, error) {
		if err := decodeParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return c.usage.Refresh(ctx)
	}},
	"Usage.Watch": {watch: func(ctx context.Context, c call, params json.RawMessage, push func(any) error) error {
		if err := decodeParams(params, &struct{}{}); err != nil {
			return err
		}
		return c.usage.Watch(ctx, func(s usage.ReadState) error { return push(s) })
	}},
}

// Methods 是遠程開放的方法名稱，排序過。
func Methods() []string { return slices.Sorted(maps.Keys(methods)) }

// decodeParams 嚴格解參數：不認得的欄位、型別不對或多餘的內容都是
// remote.bad_request。沒有參數等於空物件。
func decodeParams(params json.RawMessage, v any) error {
	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage("{}")
	}
	d := json.NewDecoder(bytes.NewReader(params))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errBadRequest
	}
	if _, err := d.Token(); err != io.EOF {
		return errBadRequest
	}
	return nil
}
