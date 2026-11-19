package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/task"
)

func toInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}

func toBytes(v any) []byte {
	b, _ := v.([]byte)
	return b
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func parseAt(s string) (time.Time, error) {
	t, err := time.Parse(timeFmt, s)
	if err != nil {
		return t, fail(CodeFailed, err, "detail", err.Error())
	}
	return t, nil
}

// parseOptionalAt 是 formatOptionalAt 的反向：空字串是零值。
func parseOptionalAt(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return parseAt(s)
}

// Workspaces 回傳全部 Workspace。
func (s *Store) Workspaces(ctx context.Context) ([]hub.Workspace, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx,
		"SELECT id, path, name, policy, last_used_at FROM workspaces ORDER BY path")
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]hub.Workspace, 0, len(rows.Values))
	for _, v := range rows.Values {
		at, err := parseAt(toStr(v[4]))
		if err != nil {
			return nil, err
		}
		out = append(out, hub.Workspace{
			ID: task.WorkspaceID(toStr(v[0])), Path: toStr(v[1]), Name: toStr(v[2]),
			Policy: task.IsolationPolicy(toStr(v[3])), LastUsedAt: at})
	}
	return out, nil
}

// PutWorkspace upsert 一個 Workspace。
func (s *Store) PutWorkspace(ctx context.Context, w hub.Workspace) error {
	if err := s.check(); err != nil {
		return err
	}
	_, err := s.w.Execute(ctx,
		"INSERT OR REPLACE INTO workspaces(id, path, name, policy, last_used_at) VALUES(?,?,?,?,?)",
		string(w.ID), w.Path, w.Name, string(w.Policy), formatAt(w.LastUsedAt))
	return mapErr(err)
}

// Rules 回傳全部 Rule。
func (s *Store) Rules(ctx context.Context) ([]task.Rule, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx,
		"SELECT id, scope, scope_id, agent, match, tool, pattern, verdict FROM rules ORDER BY id")
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]task.Rule, 0, len(rows.Values))
	for _, v := range rows.Values {
		out = append(out, task.Rule{
			ID: task.RuleID(toStr(v[0])), Scope: task.RuleScope(toStr(v[1])),
			ScopeID: toStr(v[2]), Agent: task.AgentID(toStr(v[3])),
			Match: task.RuleMatch(toStr(v[4])), Tool: toStr(v[5]),
			Pattern: toStr(v[6]), Verdict: task.Verdict(toStr(v[7]))})
	}
	return out, nil
}

// DeleteRule 移除一條 Rule；不存在不算錯。
func (s *Store) DeleteRule(ctx context.Context, id task.RuleID) error {
	if err := s.check(); err != nil {
		return err
	}
	_, err := s.w.Execute(ctx, "DELETE FROM rules WHERE id=?", string(id))
	return mapErr(err)
}

const settingsKey = "hub"

// Settings 回傳設定：先填 DefaultSettings，再把存過的 JSON 解上去，
// 因此沒有存過或缺少的欄位都是預設值。
func (s *Store) Settings(ctx context.Context) (hub.Settings, error) {
	out := hub.DefaultSettings()
	if err := s.check(); err != nil {
		return out, err
	}
	rows, err := s.r.Query(ctx,
		"SELECT value_json FROM settings WHERE key=?", settingsKey)
	if err != nil {
		return out, mapErr(err)
	}
	if len(rows.Values) == 0 {
		return out, nil
	}
	if err := json.Unmarshal([]byte(toStr(rows.Values[0][0])), &out); err != nil {
		return out, fail(CodeFailed, err, "detail", err.Error())
	}
	return out, nil
}

// PutSettings 寫入設定。
func (s *Store) PutSettings(ctx context.Context, x hub.Settings) error {
	if err := s.check(); err != nil {
		return err
	}
	b, err := json.Marshal(x)
	if err != nil {
		return fail(CodeFailed, err, "detail", err.Error())
	}
	_, err = s.w.Execute(ctx,
		"INSERT OR REPLACE INTO settings(key, value_json) VALUES(?,?)", settingsKey, string(b))
	return mapErr(err)
}

// Labels 回傳標籤目錄（D135）。
func (s *Store) Labels(ctx context.Context) ([]hub.Label, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx, "SELECT id, name, tone FROM labels ORDER BY id")
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]hub.Label, 0, len(rows.Values))
	for _, v := range rows.Values {
		out = append(out, hub.Label{ID: task.LabelID(toStr(v[0])), Name: toStr(v[1]), Tone: hub.LabelTone(toStr(v[2]))})
	}
	return out, nil
}

// PutLabel upsert 一個標籤。
func (s *Store) PutLabel(ctx context.Context, l hub.Label) error {
	if err := s.check(); err != nil {
		return err
	}
	_, err := s.w.Execute(ctx, "INSERT OR REPLACE INTO labels(id, name, tone) VALUES(?,?,?)",
		string(l.ID), l.Name, string(l.Tone))
	return mapErr(err)
}

// DeleteLabel 刪掉一個標籤；不存在不算錯。
func (s *Store) DeleteLabel(ctx context.Context, id task.LabelID) error {
	if err := s.check(); err != nil {
		return err
	}
	_, err := s.w.Execute(ctx, "DELETE FROM labels WHERE id=?", string(id))
	return mapErr(err)
}

// Devices 回傳配對過的遠程設備（D145），按配對時間、同刻按 ID。
func (s *Store) Devices(ctx context.Context) ([]hub.Device, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.r.Query(ctx, "SELECT id, name, key, paired_at, last_seen FROM devices ORDER BY paired_at, id")
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]hub.Device, 0, len(rows.Values))
	for _, v := range rows.Values {
		d := hub.Device{ID: task.DeviceID(toStr(v[0])), Name: toStr(v[1])}
		if err := d.Key.UnmarshalText([]byte(toStr(v[2]))); err != nil {
			return nil, fail(CodeFailed, err, "detail", err.Error())
		}
		if d.PairedAt, err = parseAt(toStr(v[3])); err != nil {
			return nil, err
		}
		if d.LastSeen, err = parseOptionalAt(toStr(v[4])); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// PutDevice upsert 一台設備。
func (s *Store) PutDevice(ctx context.Context, d hub.Device) error {
	if err := s.check(); err != nil {
		return err
	}
	_, err := s.w.Execute(ctx,
		"INSERT OR REPLACE INTO devices(id, name, key, paired_at, last_seen) VALUES(?,?,?,?,?)",
		string(d.ID), d.Name, d.Key.String(), formatAt(d.PairedAt), formatOptionalAt(d.LastSeen))
	return mapErr(err)
}

// DeleteDevice 刪掉一台設備；不存在不算錯。
func (s *Store) DeleteDevice(ctx context.Context, id task.DeviceID) error {
	if err := s.check(); err != nil {
		return err
	}
	_, err := s.w.Execute(ctx, "DELETE FROM devices WHERE id=?", string(id))
	return mapErr(err)
}

// ClearDevices 刪掉所有設備（重置遠程）。
func (s *Store) ClearDevices(ctx context.Context) error {
	if err := s.check(); err != nil {
		return err
	}
	_, err := s.w.Execute(ctx, "DELETE FROM devices")
	return mapErr(err)
}
