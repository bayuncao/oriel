package hub

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bayuncao/oriel/internal/task"
)

// Label 是全局標籤目錄裡的一個標籤（D135）：所有 Workspace 的 Task 共用，Task 只記它的 ID
// （task.State.Labels），名稱與色調改了，帶它的 Task 一起變。目錄是跨 Task 的設定，經 Config 存取。
type Label struct {
	ID   task.LabelID `json:"id"`
	Name string       `json:"name"`
	Tone LabelTone    `json:"tone"`
}

// LabelTone 是標籤的色調，照 Arc 看板的七種。
type LabelTone string

const (
	ToneTeal   LabelTone = "teal"
	ToneSky    LabelTone = "sky"
	ToneOrchid LabelTone = "orchid"
	ToneViolet LabelTone = "violet"
	ToneRose   LabelTone = "rose"
	ToneAmber  LabelTone = "amber"
	ToneGreen  LabelTone = "green"
)

// LabelNameMax 是標籤名稱最多的字數（Unicode 字元，m2.md B09）。
const LabelNameMax = 40

// Labels 是標籤目錄，按 ID 排序（顯示的順序由頁面按界面語言排）。
func (h *Hub) Labels(ctx context.Context, p Principal) ([]Label, error) {
	if err := h.alive(); err != nil {
		return nil, err
	}
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	return sortedLabels(h.cfg.labels), nil
}

// PutLabel 新建（ID 為空）或改掉一個標籤，回傳存下的標籤（m2.md B07–B10）。名稱去首尾空白，不能為空、
// 不能含換行、最多 LabelNameMax 字（hub.invalid_field{name}），也不能與別的標籤同名（不分大小寫，
// hub.duplicate_label{name}）；色調不是七種之一是 hub.invalid_field{tone}；改的標籤不在是
// hub.unknown_label{label}。只有桌面能用。
func (h *Hub) PutLabel(ctx context.Context, p Principal, l Label) (Label, error) {
	if err := h.alive(); err != nil {
		return Label{}, err
	}
	if err := h.needsDesktop(p); err != nil {
		return Label{}, err
	}
	name, ok := task.Label(l.Name)
	if !ok || utf8.RuneCountInString(name) > LabelNameMax {
		return Label{}, fail(CodeInvalidField, nil, "field", "name")
	}
	l.Name = name
	switch l.Tone {
	case ToneTeal, ToneSky, ToneOrchid, ToneViolet, ToneRose, ToneAmber, ToneGreen:
	default:
		return Label{}, fail(CodeInvalidField, nil, "field", "tone")
	}
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	h.cfg.mu.Lock()
	_, known := h.cfg.labels[l.ID]
	var clash *Label
	for _, x := range h.cfg.labels {
		if x.ID != l.ID && strings.EqualFold(x.Name, l.Name) {
			clash = &x
			break
		}
	}
	h.cfg.mu.Unlock()
	switch {
	case l.ID != "" && !known:
		return Label{}, fail(CodeUnknownLabel, nil, "label", string(l.ID))
	case clash != nil:
		return Label{}, fail(CodeDuplicateLabel, nil, "name", clash.Name)
	case l.ID == "":
		l.ID = task.LabelID(newID("l-"))
	}
	if err := h.config.PutLabel(ctx, l); err != nil {
		return Label{}, fail(CodeStoreFailed, err, "detail", err.Error())
	}
	h.cfg.mu.Lock()
	h.cfg.labels[l.ID] = l
	h.cfg.mu.Unlock()
	return l, nil
}

// DeleteLabel 從目錄刪掉一個標籤（m2.md B11）；帶著它的 Task 不改，讀端略過目錄裡沒有的 ID，之後再建的
// 同名標籤是新的 ID，不會回到舊 Task 上。未知 ID 是 hub.unknown_label。只有桌面能用。
func (h *Hub) DeleteLabel(ctx context.Context, p Principal, id task.LabelID) error {
	if err := h.alive(); err != nil {
		return err
	}
	if err := h.needsDesktop(p); err != nil {
		return err
	}
	h.cfgWrite.Lock()
	defer h.cfgWrite.Unlock()
	h.cfg.mu.Lock()
	_, ok := h.cfg.labels[id]
	h.cfg.mu.Unlock()
	if !ok {
		return fail(CodeUnknownLabel, nil, "label", string(id))
	}
	if err := h.config.DeleteLabel(ctx, id); err != nil {
		return fail(CodeStoreFailed, err, "detail", err.Error())
	}
	h.cfg.mu.Lock()
	delete(h.cfg.labels, id)
	h.cfg.mu.Unlock()
	return nil
}

// labelIDs 是目錄裡現有標籤的 ID，給 set_labels 的 Inputs.Labels。
func (h *Hub) labelIDs() []task.LabelID {
	h.cfg.mu.Lock()
	defer h.cfg.mu.Unlock()
	return slices.Sorted(maps.Keys(h.cfg.labels))
}

func sortedLabels(m map[task.LabelID]Label) []Label {
	return slices.SortedFunc(maps.Values(m), func(a, b Label) int { return cmp.Compare(a.ID, b.ID) })
}
