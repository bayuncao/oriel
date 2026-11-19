package hub

import "github.com/bayuncao/oriel/internal/task"

// Principal 是發出請求的人：desktop 上的本地擁有者，或
// 已配對的 Device。hub 依它授權並衍生 Command 的 Origin。
type Principal struct {
	Device task.DeviceID `json:"device,omitzero"` // 本地擁有者時為空
}

// LocalOwner 是 desktop 使用者。
var LocalOwner = Principal{}

func (p Principal) Origin() task.Origin {
	if p.Device == "" {
		return task.OriginDesktop
	}
	return task.OriginRemote
}
