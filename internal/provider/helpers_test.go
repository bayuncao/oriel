package provider

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// 測試的時區與“現在”：2026-11-13 12:00 UTC+8。
var (
	zone = time.FixedZone("UTC+8", 8*3600)
	now  = at(13, 12, 0)
)

func at(day, hour, minute int) time.Time { return time.Date(2026, 11, day, hour, minute, 0, 0, zone) }

type comp struct{ name, status string }

// summaryJSON 是 Statuspage 的 summary.json。
func summaryJSON(indicator string, comps ...comp) []byte {
	list := []map[string]any{}
	for i, c := range comps {
		list = append(list, map[string]any{"id": fmt.Sprint("c", i), "name": c.name, "status": c.status, "group": false, "group_id": nil})
	}
	return mustJSON(map[string]any{"status": map[string]any{"indicator": indicator}, "components": list})
}

type aff struct{ name, old, new string }

type upd struct {
	status   string
	at       time.Time
	body     string
	affected []aff
}

type inc struct {
	id, name, status, impact string
	start, end               time.Time // end 為零值：沒有 resolved_at
	comps                    []string
	updates                  []upd
	shortlink                string
}

// incidentsJSON 是 Statuspage 的 incidents.json。
func incidentsJSON(list ...inc) []byte {
	out := []map[string]any{}
	for _, in := range list {
		comps := []map[string]any{}
		for _, c := range in.comps {
			comps = append(comps, map[string]any{"id": c, "name": c, "status": "operational"})
		}
		updates := []map[string]any{}
		for _, u := range in.updates {
			affected := []map[string]any{}
			for _, a := range u.affected {
				affected = append(affected, map[string]any{"code": a.name, "name": a.name, "old_status": a.old, "new_status": a.new})
			}
			updates = append(updates, map[string]any{"status": u.status, "body": u.body, "created_at": u.at, "display_at": u.at, "affected_components": affected})
		}
		m := map[string]any{"id": in.id, "name": in.name, "status": in.status, "impact": in.impact,
			"created_at": in.start, "started_at": in.start, "updated_at": in.start, "shortlink": in.shortlink,
			"components": comps, "incident_updates": updates, "resolved_at": nil}
		if !in.end.IsZero() {
			m["resolved_at"] = in.end
			m["updated_at"] = in.end
		}
		out = append(out, m)
	}
	return mustJSON(map[string]any{"incidents": out})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// dayOf 是 30 天裡的某一天。
func dayOf(t *testing.T, list []ProviderDay, date string) ProviderDay {
	t.Helper()
	for _, d := range list {
		if d.Date == date {
			return d
		}
	}
	t.Fatalf("no day %s in %d days", date, len(list))
	return ProviderDay{}
}

func uptime(d ProviderDay) string {
	if d.Uptime == nil {
		return "nil"
	}
	return fmt.Sprintf("%.3f", *d.Uptime)
}
