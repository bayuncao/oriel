package provider

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// reading 是讀一家得到的結果，還沒按天算。
type reading struct {
	health     Health
	components []string
	wholePage  bool
	history    history
	incident   *ProviderIncident
}

// truncatedAt：Statuspage 的事故列表最多給 50 條，incident.io 給 25 條；少於 25 條
// 時列表是完整的（PS13）。
const truncatedAt = 25

type spComponent struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Group      bool     `json:"group"`
	GroupID    *string  `json:"group_id"`
	Components []string `json:"components"`
}

type spSummary struct {
	Status struct {
		Indicator string `json:"indicator"`
	} `json:"status"`
	Components []spComponent `json:"components"`
}

type spAffected struct {
	Name      string `json:"name"`
	OldStatus string `json:"old_status"`
	NewStatus string `json:"new_status"`
}

type spUpdate struct {
	Status    string       `json:"status"`
	Body      string       `json:"body"`
	CreatedAt time.Time    `json:"created_at"`
	DisplayAt *time.Time   `json:"display_at"`
	Affected  []spAffected `json:"affected_components"`
}

func (u spUpdate) at() time.Time {
	if u.DisplayAt != nil && !u.DisplayAt.IsZero() {
		return *u.DisplayAt
	}
	return u.CreatedAt
}

type spIncident struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Status     string        `json:"status"`
	Impact     string        `json:"impact"`
	Shortlink  string        `json:"shortlink"`
	CreatedAt  time.Time     `json:"created_at"`
	StartedAt  *time.Time    `json:"started_at"`
	ResolvedAt *time.Time    `json:"resolved_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	Components []spComponent `json:"components"`
	Updates    []spUpdate    `json:"incident_updates"`
}

func (i spIncident) start() time.Time {
	if i.StartedAt != nil && !i.StartedAt.IsZero() {
		return *i.StartedAt
	}
	return i.CreatedAt
}

// end 是事故解決的時間；沒解決時為零值。incident.io 不給 resolved_at，取 resolved
// 那次更新的時間。
func (i spIncident) end() time.Time {
	if i.ResolvedAt != nil && !i.ResolvedAt.IsZero() {
		return *i.ResolvedAt
	}
	if !resolved(i.Status) {
		return time.Time{}
	}
	var at time.Time
	for _, u := range i.Updates {
		if resolved(u.Status) && (at.IsZero() || u.at().Before(at)) {
			at = u.at()
		}
	}
	if at.IsZero() {
		at = i.UpdatedAt
	}
	return at
}

func resolved(status string) bool { return status == "resolved" || status == "postmortem" }

// componentHealth：性能下降、部分中斷與維護算 degraded，完全中斷算 outage（PS05、PS06）。
func componentHealth(status string) (Health, float64) {
	switch status {
	case "operational", "":
		return HealthOperational, weightNone
	case "major_outage":
		return HealthOutage, weightOutage
	case "partial_outage":
		return HealthDegraded, weightPartial
	default: // degraded_performance、under_maintenance
		return HealthDegraded, weightNone
	}
}

// impactHealth 是沒寫組件的事故按影響程度算（PS12）；none 不算。
func impactHealth(impact string) (Health, float64, bool) {
	switch impact {
	case "critical":
		return HealthOutage, weightOutage, true
	case "major":
		return HealthDegraded, weightPartial, true
	case "minor":
		return HealthDegraded, weightNone, true
	}
	return "", 0, false
}

// indicatorHealth 是整頁的指標（Track 為 nil 時的當前狀態）。
func indicatorHealth(indicator string) Health {
	switch indicator {
	case "none":
		return HealthOperational
	case "critical":
		return HealthOutage
	default: // minor、major（部分中斷）
		return HealthDegraded
	}
}

// parseStatuspage 讀 summary.json 與 incidents.json。
func parseStatuspage(v Vendor, summaryJSON, incidentsJSON []byte, now time.Time) (reading, error) {
	var sum spSummary
	if err := json.Unmarshal(summaryJSON, &sum); err != nil {
		return reading{}, fmt.Errorf("summary.json: %w", err)
	}
	var inc struct {
		Incidents *[]spIncident `json:"incidents"`
	}
	if err := json.Unmarshal(incidentsJSON, &inc); err != nil {
		return reading{}, fmt.Errorf("incidents.json: %w", err)
	}
	if inc.Incidents == nil {
		return reading{}, fmt.Errorf("incidents.json: no incidents list")
	}
	incidents := *inc.Incidents

	var r reading
	tracked := map[string]bool{} // 算進來的組件名稱，組件組連同其中的組件
	if v.Track == nil {
		if sum.Status.Indicator == "" {
			return reading{}, fmt.Errorf("summary.json: no status indicator")
		}
		r.health = indicatorHealth(sum.Status.Indicator)
	} else {
		byID := map[string]spComponent{}
		for _, c := range sum.Components {
			byID[c.ID] = c
		}
		r.health = HealthOperational
		for _, c := range sum.Components {
			if !v.Track(c.Name) {
				continue
			}
			r.components = append(r.components, c.Name)
			list := []spComponent{c}
			for _, id := range c.Components {
				if child, ok := byID[id]; ok {
					list = append(list, child)
				}
			}
			for _, c := range list {
				tracked[c.Name] = true
				h, _ := componentHealth(c.Status)
				r.health = worse(r.health, h)
			}
		}
		if len(r.components) == 0 {
			return reading{}, fmt.Errorf("summary.json: none of the tracked components is listed")
		}
	}

	r.history.known = true
	r.history.complete = len(incidents) < truncatedAt
	var latest *spIncident
	for i := range incidents {
		in := incidents[i]
		if r.history.since.IsZero() || in.start().Before(r.history.since) {
			r.history.since = in.start()
		}
		spans, counted, whole := incidentSpans(in, v.Track, tracked, now)
		if !counted {
			continue
		}
		// 沒寫組件、影響為 none 的事故不算停機，這一行也就不是按整頁算的。
		r.wholePage = r.wholePage || v.Track == nil || (whole && len(spans) > 0)
		r.history.spans = append(r.history.spans, spans...)
		if shown(in.end(), now) && (latest == nil || in.start().After(latest.start())) {
			latest = &incidents[i]
		}
	}
	if latest != nil {
		r.incident = statuspageIncident(v, *latest)
	}
	return r, nil
}

// shown：沒解決的，或 24 小時內解決的事故在一行裡展開（PS07）。
func shown(end, now time.Time) bool { return end.IsZero() || now.Sub(end) <= recentWindow }

// incidentSpans 算一次事故的停機。counted：這次事故算進這一行；whole：它沒寫組件，
// 算的是整頁。寫了組件變化的只算追蹤的組件（PS11）；其餘按影響程度（PS12）。
func incidentSpans(in spIncident, track func(string) bool, tracked map[string]bool, now time.Time) (spans []span, counted, whole bool) {
	end := in.end()
	if end.IsZero() {
		end = now
	}
	impact := func() []span {
		h, w, ok := impactHealth(in.Impact)
		if !ok {
			return nil
		}
		return []span{{from: in.start(), to: end, weight: w, health: h}}
	}
	if track == nil || len(in.Components) == 0 {
		return impact(), true, true
	}
	if !slices.ContainsFunc(in.Components, func(c spComponent) bool { return tracked[c.Name] }) {
		return nil, false, false
	}
	updates := slices.Clone(in.Updates)
	slices.SortStableFunc(updates, func(a, b spUpdate) int { return a.at().Compare(b.at()) })
	type open struct {
		since  time.Time
		status string
	}
	state := map[string]open{}
	changed := false
	for _, u := range updates {
		for _, a := range u.Affected {
			if !tracked[a.Name] {
				continue
			}
			changed = true
			cur, seen := state[a.Name]
			if !seen {
				cur = open{since: in.start(), status: a.OldStatus}
			}
			spans = appendStatus(spans, cur.since, u.at(), cur.status)
			state[a.Name] = open{since: u.at(), status: a.NewStatus}
		}
	}
	if !changed {
		return impact(), true, false
	}
	for _, cur := range state {
		spans = appendStatus(spans, cur.since, end, cur.status)
	}
	return spans, true, false
}

func appendStatus(spans []span, from, to time.Time, status string) []span {
	h, w := componentHealth(status)
	if h == HealthOperational || !to.After(from) {
		return spans
	}
	return append(spans, span{from: from, to: to, weight: w, health: h})
}

func statuspageIncident(v Vendor, in spIncident) *ProviderIncident {
	url := in.Shortlink
	if url == "" {
		url = strings.TrimRight(v.Page, "/") + "/incidents/" + in.ID
	}
	updates := slices.Clone(in.Updates)
	slices.SortStableFunc(updates, func(a, b spUpdate) int { return a.at().Compare(b.at()) })
	out := &ProviderIncident{ID: in.ID, Title: in.Name, URL: url}
	stage := StageInvestigating
	for _, u := range updates {
		stage = stageOf(u.Status, stage)
		out.Updates = append(out.Updates, IncidentUpdate{Stage: stage, At: u.at(), Message: strings.TrimSpace(u.Body)})
	}
	slices.Reverse(out.Updates)
	return out
}

// stageOf 對應一次更新的狀態；沒有對應的（如 incident.io 的 update）沿用前一個階段。
func stageOf(status string, previous IncidentStage) IncidentStage {
	switch status {
	case "investigating":
		return StageInvestigating
	case "identified":
		return StageIdentified
	case "monitoring":
		return StageMonitoring
	case "resolved", "postmortem":
		return StageResolved
	}
	return previous
}
