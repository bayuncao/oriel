package provider

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

var tracked = Vendor{ID: "v", Name: "Vendor", Page: "https://status.example.com", Feed: FeedStatuspage, Track: named("API", "Code")}

func readStatuspage(t *testing.T, v Vendor, sum, incidents []byte) reading {
	t.Helper()
	r, err := parseStatuspage(v, sum, incidents, now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// PS04–PS06：當前狀態取算進來的組件裡最差的；Track 為 nil 時取整頁的指標。
func TestCurrentHealth(t *testing.T) {
	tests := []struct {
		name      string
		indicator string
		comps     []comp
		whole     bool // Track 為 nil
		want      Health
	}{
		{"PS04 all operational", "minor", []comp{{"API", "operational"}, {"Code", "operational"}, {"Web", "major_outage"}}, false, HealthOperational},
		{"PS05 degraded performance", "none", []comp{{"API", "degraded_performance"}, {"Code", "operational"}}, false, HealthDegraded},
		{"PS05 partial outage", "none", []comp{{"API", "operational"}, {"Code", "partial_outage"}}, false, HealthDegraded},
		{"PS05 maintenance", "none", []comp{{"API", "under_maintenance"}, {"Code", "operational"}}, false, HealthDegraded},
		{"PS06 major outage wins", "none", []comp{{"API", "partial_outage"}, {"Code", "major_outage"}}, false, HealthOutage},
		{"whole page none", "none", []comp{{"Model A", "major_outage"}}, true, HealthOperational},
		{"whole page minor", "minor", nil, true, HealthDegraded},
		{"whole page major is a partial outage", "major", nil, true, HealthDegraded},
		{"whole page critical", "critical", nil, true, HealthOutage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tracked
			if tt.whole {
				v.Track = nil
			}
			r := readStatuspage(t, v, summaryJSON(tt.indicator, tt.comps...), incidentsJSON())
			if r.health != tt.want {
				t.Errorf("health = %q, want %q", r.health, tt.want)
			}
		})
	}
}

// PS01：名稱下面那一行是算進來的組件；整頁時為空。
func TestComponents(t *testing.T) {
	r := readStatuspage(t, tracked, summaryJSON("none", comp{"Web", "operational"}, comp{"Code", "operational"}, comp{"API", "operational"}), incidentsJSON())
	if !slices.Equal(r.components, []string{"Code", "API"}) {
		t.Errorf("components = %q", r.components)
	}
	whole := tracked
	whole.Track = nil
	if r := readStatuspage(t, whole, summaryJSON("none", comp{"Web", "operational"}), incidentsJSON()); len(r.components) != 0 {
		t.Errorf("whole page components = %q", r.components)
	}
	if _, err := parseStatuspage(tracked, summaryJSON("none", comp{"Web", "operational"}), incidentsJSON(), now); err == nil {
		t.Error("tracked components missing from the summary: want an error")
	}
}

// PS11–PS15：按天算在線率與顏色。
func TestDays(t *testing.T) {
	ok := summaryJSON("none", comp{"API", "operational"}, comp{"Code", "operational"})
	tests := []struct {
		name      string
		track     bool // false：整頁
		incidents []inc
		want      map[string]string // 日期 → "健康 在線率"
		wholePage bool
	}{
		{
			name: "PS11 component changes: major counts fully, partial 30%",
			incidents: []inc{{id: "a", status: "resolved", impact: "major", start: at(12, 10, 0), end: at(12, 12, 0), comps: []string{"API", "Web"},
				updates: []upd{
					{status: "investigating", at: at(12, 10, 0), affected: []aff{{"API", "operational", "major_outage"}, {"Web", "operational", "major_outage"}}},
					{status: "monitoring", at: at(12, 11, 0), affected: []aff{{"API", "major_outage", "partial_outage"}}},
					{status: "resolved", at: at(12, 12, 0), affected: []aff{{"API", "partial_outage", "operational"}}},
				}}},
			// 1 h × 1 + 1 h × 0.3 = 1.3 h；Web 不算。
			want: map[string]string{"2026-11-12": "outage 94.583", "2026-11-11": "operational 100.000"},
		},
		{
			name: "PS11 degraded performance only marks the day",
			incidents: []inc{{id: "a", status: "resolved", impact: "minor", start: at(12, 10, 0), end: at(12, 14, 0), comps: []string{"Code"},
				updates: []upd{{status: "investigating", at: at(12, 10, 0), affected: []aff{{"Code", "operational", "degraded_performance"}}}}}},
			want: map[string]string{"2026-11-12": "degraded 100.000"},
		},
		{
			name: "PS11 incident on an untracked component is not counted",
			incidents: []inc{{id: "a", status: "resolved", impact: "critical", start: at(12, 10, 0), end: at(12, 14, 0), comps: []string{"Web"},
				updates: []upd{{status: "investigating", at: at(12, 10, 0), affected: []aff{{"Web", "operational", "major_outage"}}}}}},
			want: map[string]string{"2026-11-12": "operational 100.000"},
		},
		{
			name: "PS11 tracked incident without component changes falls back to impact",
			incidents: []inc{{id: "a", status: "resolved", impact: "critical", start: at(12, 10, 0), end: at(12, 12, 0), comps: []string{"API"},
				updates: []upd{{status: "investigating", at: at(12, 10, 0)}}}},
			want: map[string]string{"2026-11-12": "outage 91.667"},
		},
		{
			name: "PS12 incidents without components count by impact",
			incidents: []inc{
				{id: "a", status: "resolved", impact: "critical", start: at(10, 10, 0), end: at(10, 12, 0)},
				{id: "b", status: "resolved", impact: "major", start: at(11, 10, 0), end: at(11, 11, 0)},
				{id: "c", status: "resolved", impact: "minor", start: at(12, 10, 0), end: at(12, 18, 0)},
				{id: "d", status: "resolved", impact: "none", start: at(9, 10, 0), end: at(9, 18, 0)},
			},
			want:      map[string]string{"2026-11-10": "outage 91.667", "2026-11-11": "degraded 98.750", "2026-11-12": "degraded 100.000", "2026-11-09": "operational 100.000"},
			wholePage: true,
		},
		{
			name:  "PS12 whole page counts every incident",
			track: false,
			incidents: []inc{{id: "a", status: "resolved", impact: "critical", start: at(10, 10, 0), end: at(10, 12, 0), comps: []string{"Web"},
				updates: []upd{{status: "investigating", at: at(10, 10, 0), affected: []aff{{"Web", "operational", "major_outage"}}}}}},
			want:      map[string]string{"2026-11-10": "outage 91.667"},
			wholePage: true,
		},
		{
			name:      "PS13 a complete list covers all 30 days",
			incidents: []inc{{id: "a", status: "resolved", impact: "minor", start: at(10, 10, 0), end: at(10, 11, 0)}},
			want:      map[string]string{"2026-10-15": "operational 100.000", "2026-11-10": "degraded 100.000"},
			wholePage: true,
		},
		{
			name:      "PS14 an incident across midnight counts on both days",
			incidents: []inc{{id: "a", status: "resolved", impact: "critical", start: at(11, 23, 0), end: at(12, 1, 0)}},
			want:      map[string]string{"2026-11-11": "outage 95.833", "2026-11-12": "outage 95.833"},
			wholePage: true,
		},
		{
			name:      "PS15 today counts up to now; an open incident lasts until now",
			incidents: []inc{{id: "a", status: "investigating", impact: "critical", start: at(13, 11, 0)}},
			want:      map[string]string{"2026-11-13": "outage 91.667"},
			wholePage: true,
		},
		{
			name: "overlapping incidents are not counted twice",
			incidents: []inc{
				{id: "a", status: "resolved", impact: "critical", start: at(12, 10, 0), end: at(12, 12, 0)},
				{id: "b", status: "resolved", impact: "major", start: at(12, 11, 0), end: at(12, 13, 0)},
			},
			// 10–12 全算，12–13 算 30%：2.3 h。
			want:      map[string]string{"2026-11-12": "outage 90.417"},
			wholePage: true,
		},
		{
			name: "incident.io: no resolved_at, the resolved update ends it",
			incidents: []inc{{id: "a", status: "resolved", impact: "critical", start: at(12, 10, 0), updates: []upd{
				{status: "investigating", at: at(12, 10, 0)}, {status: "resolved", at: at(12, 12, 0)}}}},
			want:      map[string]string{"2026-11-12": "outage 91.667"},
			wholePage: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tracked
			if strings.HasPrefix(tt.name, "PS12 whole page") {
				v.Track = nil
			}
			r := readStatuspage(t, v, ok, incidentsJSON(tt.incidents...))
			if r.wholePage != tt.wholePage {
				t.Errorf("wholePage = %v, want %v", r.wholePage, tt.wholePage)
			}
			list := days(r.history, now, zone)
			if len(list) != 30 || list[0].Date != "2026-10-15" || list[29].Date != "2026-11-13" {
				t.Fatalf("days run %s–%s (%d)", list[0].Date, list[len(list)-1].Date, len(list))
			}
			for date, want := range tt.want {
				d := dayOf(t, list, date)
				if got := string(d.Health) + " " + uptime(d); got != want {
					t.Errorf("%s = %s, want %s", date, got, want)
				}
			}
		})
	}
}

// PS13：事故列表被截斷時，最早一條之前的日子無數據。
func TestTruncatedListLeavesEarlierDaysEmpty(t *testing.T) {
	var list []inc
	for i := range truncatedAt {
		list = append(list, inc{id: string(rune('a' + i)), status: "resolved", impact: "minor", start: at(13, 0, i), end: at(13, 0, i+1)})
	}
	list[len(list)-1].start, list[len(list)-1].end = at(1, 9, 0), at(1, 10, 0)
	r := readStatuspage(t, tracked, summaryJSON("none", comp{"API", "operational"}), incidentsJSON(list...))
	got := days(r.history, now, zone)
	if d := dayOf(t, got, "2026-10-31"); d.Uptime != nil || d.Health != "" {
		t.Errorf("before the oldest incident: %+v, want no data", d)
	}
	if d := dayOf(t, got, "2026-11-01"); d.Uptime == nil || d.Health != HealthDegraded {
		t.Errorf("the oldest incident's day: %s %s, want degraded with data", d.Health, uptime(d))
	}
}

// PS07：沒解決的或 24 小時內解決的事故可以展開；更新新的在前，沒有對應階段的沿用前一個。
func TestIncident(t *testing.T) {
	ok := summaryJSON("none", comp{"API", "operational"})
	updates := []upd{
		{status: "investigating", at: at(13, 9, 0), body: "Looking into it"},
		{status: "update", at: at(13, 9, 30), body: "Still looking"},
		{status: "identified", at: at(13, 10, 0), body: "Found it"},
	}
	tests := []struct {
		name   string
		in     inc
		wantID string
	}{
		{"open", inc{id: "open", name: "Errors", status: "identified", impact: "major", start: at(13, 9, 0), updates: updates}, "open"},
		{"resolved 23 hours ago", inc{id: "recent", status: "resolved", impact: "minor", start: at(12, 12, 0), end: at(12, 13, 0)}, "recent"},
		{"resolved 25 hours ago", inc{id: "old", status: "resolved", impact: "minor", start: at(12, 10, 0), end: at(12, 11, 0)}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := readStatuspage(t, tracked, ok, incidentsJSON(tt.in))
			if tt.wantID == "" {
				if r.incident != nil {
					t.Errorf("incident = %+v, want none", r.incident)
				}
				return
			}
			if r.incident == nil || r.incident.ID != tt.wantID {
				t.Fatalf("incident = %+v, want %s", r.incident, tt.wantID)
			}
		})
	}
	r := readStatuspage(t, tracked, ok, incidentsJSON(inc{id: "open", name: "Errors", status: "identified", impact: "major", start: at(13, 9, 0), updates: updates}))
	var stages []IncidentStage
	for _, u := range r.incident.Updates {
		stages = append(stages, u.Stage)
	}
	if want := []IncidentStage{StageIdentified, StageInvestigating, StageInvestigating}; !slices.Equal(stages, want) {
		t.Errorf("stages = %v, want %v", stages, want)
	}
	if r.incident.Updates[0].Message != "Found it" || r.incident.URL != "https://status.example.com/incidents/open" {
		t.Errorf("incident = %+v", r.incident)
	}
	r = readStatuspage(t, tracked, ok, incidentsJSON(inc{id: "x", status: "investigating", impact: "minor", start: at(13, 9, 0), shortlink: "https://stspg.io/x"}))
	if r.incident.URL != "https://stspg.io/x" {
		t.Errorf("url = %q, want the shortlink", r.incident.URL)
	}
}

// 格式不對的回應是讀取失敗（PS23）。
func TestMalformedStatuspage(t *testing.T) {
	ok := summaryJSON("none", comp{"API", "operational"})
	for name, c := range map[string]struct{ sum, inc string }{
		"summary not JSON":   {"<html>", string(incidentsJSON())},
		"incidents not JSON": {string(ok), "<html>"},
		"no incidents list":  {string(ok), `{"page":{}}`},
		"no indicator":       {`{"components":[]}`, string(incidentsJSON())},
	} {
		v := tracked
		if name == "no indicator" {
			v.Track = nil
		}
		if _, err := parseStatuspage(v, []byte(c.sum), []byte(c.inc), now); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// PS08、PS16：DeepSeek 的 RSS。
func TestFlashduty(t *testing.T) {
	item := func(title, status, components string, pub time.Time) string {
		return `<item><title>` + title + `</title><link>https://status.deepseek.com/incidents/` + title + `</link>` +
			`<description>&lt;p&gt;&lt;strong&gt;Status:&lt;/strong&gt; ` + status + `&lt;/p&gt;&lt;p&gt;Message ` + title + `&lt;/p&gt;` +
			`&lt;p&gt;&lt;strong&gt;Affected components:&lt;/strong&gt; ` + components + `&lt;/p&gt;</description>` +
			`<guid>urn:flashduty:change:` + title + `</guid><pubDate>` + pub.Format(time.RFC1123Z) + `</pubDate></item>`
	}
	feed := func(items ...string) []byte {
		return []byte(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>DeepSeek</title>` + strings.Join(items, "") + `</channel></rss>`)
	}
	v := Vendor{ID: "deepseek", Name: "DeepSeek", Feed: FeedFlashduty, Track: containing("API Service"), Label: []string{"API Service"}}

	r, err := parseFlashduty(v, feed(
		item("open", "investigating", "V4 API服务(API Service), 对话服务(Chat Service)", at(13, 11, 0)),
		item("chat", "resolved", "对话服务(Chat Service)", at(11, 10, 0)),
		item("old", "resolved", "V4 API服务(API Service)", at(10, 10, 0)),
	), now)
	if err != nil {
		t.Fatal(err)
	}
	if r.health != HealthDegraded {
		t.Errorf("PS08 open incident: health = %q, want degraded", r.health)
	}
	if r.incident == nil || r.incident.Title != "open" || r.incident.Updates[0].Message != "Message open" || r.incident.Updates[0].Stage != StageInvestigating {
		t.Errorf("PS08 incident = %+v", r.incident)
	}
	if !slices.Equal(r.components, []string{"V4 API服务(API Service)"}) {
		t.Errorf("components = %q", r.components)
	}
	list := days(r.history, now, zone)
	for date, want := range map[string]string{
		"2026-11-13": "degraded nil", "2026-11-11": "operational 100.000", "2026-11-10": "degraded nil", "2026-11-09": " nil",
	} {
		d := dayOf(t, list, date)
		if got := string(d.Health) + " " + uptime(d); got != want {
			t.Errorf("PS16 %s = %s, want %s", date, got, want)
		}
	}

	r, err = parseFlashduty(v, feed(item("chat", "resolved", "对话服务(Chat Service)", at(11, 10, 0))), now)
	if err != nil {
		t.Fatal(err)
	}
	if r.health != HealthOperational || r.incident != nil || !slices.Equal(r.components, []string{"API Service"}) {
		t.Errorf("PS08 nothing open: health %q, incident %+v, components %q", r.health, r.incident, r.components)
	}
	if _, err := parseFlashduty(v, []byte("<html>"), now); err == nil {
		t.Error("not RSS: want an error")
	}
}

// PS09：Google Cloud 只看 Vertex AI 上的 Gemini。
func TestGoogleCloud(t *testing.T) {
	v := Vendor{ID: "google", Name: "Google", Page: "https://status.cloud.google.com", Feed: FeedGoogleCloud, Track: named("Vertex Gemini API"), Label: []string{"Vertex Gemini API"}}
	in := func(id, product, severity string, begin, end time.Time) map[string]any {
		m := map[string]any{"id": id, "begin": begin.Format(time.RFC3339), "end": "", "severity": severity, "external_desc": "Desc " + id, "uri": "incidents/" + id,
			"affected_products": []map[string]string{{"title": product}},
			"updates":           []map[string]string{{"when": begin.Format(time.RFC3339), "text": "Investigating " + id, "status": "SERVICE_DISRUPTION"}}}
		if !end.IsZero() {
			m["end"] = end.Format(time.RFC3339)
		}
		return m
	}
	r, err := parseGoogleCloud(v, mustJSON([]any{
		in("open", "Vertex Gemini API", "high", at(13, 11, 0), time.Time{}),
		in("medium", "Vertex Gemini API", "medium", at(12, 10, 0), at(12, 12, 0)),
		in("other", "Compute Engine", "high", at(11, 10, 0), time.Time{}),
		in("oldest", "Compute Engine", "low", at(1, 10, 0), at(1, 11, 0)),
	}), now)
	if err != nil {
		t.Fatal(err)
	}
	if r.health != HealthOutage {
		t.Errorf("health = %q, want outage", r.health)
	}
	if r.incident == nil || r.incident.ID != "open" || r.incident.URL != "https://status.cloud.google.com/incidents/open" {
		t.Errorf("incident = %+v", r.incident)
	}
	list := days(r.history, now, zone)
	for date, want := range map[string]string{
		"2026-11-12": "degraded 97.500", "2026-11-11": "operational 100.000", "2026-11-13": "outage 91.667", "2026-10-31": " nil",
	} {
		d := dayOf(t, list, date)
		if got := string(d.Health) + " " + uptime(d); got != want {
			t.Errorf("%s = %s, want %s", date, got, want)
		}
	}
}

func utf16BE(s string) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFE, 0xFF})
	for _, u := range utf16.Encode([]rune(s)) {
		b.WriteByte(byte(u >> 8))
		b.WriteByte(byte(u))
	}
	return b.Bytes()
}

// PS10、PS17：AWS 只有當前事件，沒有歷史。
func TestAWSHealth(t *testing.T) {
	v := Vendor{ID: "bedrock", Name: "Amazon Bedrock", Page: "https://health.aws.amazon.com/health/status", Feed: FeedAWSHealth, Track: prefixed("bedrock-"), Label: []string{"Amazon Bedrock"}}
	event := func(service, status string, date time.Time) map[string]any {
		return map[string]any{"date": strconv.FormatInt(date.Unix(), 10), "arn": "arn:" + service + ":" + status, "status": status, "service": service, "summary": "Summary " + service,
			"event_log": []map[string]any{{"message": "First", "status": 1, "timestamp": date.Unix()}, {"message": "Second", "status": 1, "timestamp": date.Add(time.Hour).Unix()}}}
	}
	feed := func(events ...map[string]any) []byte {
		b, _ := json.Marshal(events)
		return utf16BE(string(b))
	}
	r, err := parseAWSHealth(v, feed(event("bedrock-us-east-1", "1", at(13, 9, 0)), event("bedrock-us-west-2", "0", at(13, 10, 0)), event("ec2-us-east-1", "3", at(13, 10, 0))))
	if err != nil {
		t.Fatal(err)
	}
	if r.health != HealthDegraded || r.incident == nil || r.incident.Title != "Summary bedrock-us-east-1" || r.incident.Updates[0].Message != "Second" {
		t.Errorf("PS10 health %q, incident %+v", r.health, r.incident)
	}
	for _, d := range days(r.history, now, zone) {
		if d.Uptime != nil || d.Health != "" {
			t.Fatalf("PS17 %s = %+v, want no data", d.Date, d)
		}
	}
	r, err = parseAWSHealth(v, feed(event("bedrock-us-west-2", "0", at(13, 10, 0))))
	if err != nil {
		t.Fatal(err)
	}
	if r.health != HealthOperational || r.incident != nil {
		t.Errorf("PS10 resolved only: health %q, incident %+v", r.health, r.incident)
	}
}

// 各家錄下的真實回應都讀得懂（testdata/，長的文字截短了）。
func TestRecordedFeeds(t *testing.T) {
	file := func(name string) []byte {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	catalog := map[string]Vendor{}
	for _, v := range Catalog() {
		catalog[v.ID] = v
	}
	readAt := time.Date(2026, 11, 13, 11, 0, 0, 0, zone)

	anthropic, err := parseStatuspage(catalog["anthropic"], file("anthropic_summary.json"), file("anthropic_incidents.json"), readAt)
	if err != nil {
		t.Fatal(err)
	}
	if anthropic.health != HealthOperational || !slices.Equal(anthropic.components, []string{"Claude API (api.anthropic.com)", "Claude Code"}) ||
		anthropic.wholePage || anthropic.history.complete || anthropic.history.since.In(zone).Format(time.DateOnly) != "2026-08-13" || len(anthropic.history.spans) == 0 {
		t.Errorf("anthropic: health %q, components %q, wholePage %v, complete %v, since %s, %d spans", anthropic.health, anthropic.components,
			anthropic.wholePage, anthropic.history.complete, anthropic.history.since, len(anthropic.history.spans))
	}

	openai, err := parseStatuspage(catalog["openai"], file("openai_summary.json"), file("openai_incidents.json"), readAt)
	if err != nil {
		t.Fatal(err)
	}
	if !openai.wholePage || openai.history.complete || openai.history.since.In(zone).Format(time.DateOnly) != "2026-10-21" {
		t.Errorf("openai: wholePage %v, complete %v, since %s", openai.wholePage, openai.history.complete, openai.history.since.In(zone))
	}
	if d := days(openai.history, readAt, zone); d[0].Uptime != nil || d[29].Uptime == nil {
		t.Errorf("openai: first day %+v, last day %+v; want no data before 10-21 and data today", d[0], d[29])
	}

	deepseek, err := parseFlashduty(catalog["deepseek"], file("deepseek_feed.rss"), readAt)
	if err != nil {
		t.Fatal(err)
	}
	if deepseek.health != HealthOperational || deepseek.history.since.In(zone).Format(time.DateOnly) != "2026-08-18" || len(deepseek.history.spans) == 0 ||
		!slices.Contains(deepseek.components, "DeepSeek V4 Pro API服务(API Service)") {
		t.Errorf("deepseek: health %q, since %s, %d spans, components %q", deepseek.health, deepseek.history.since, len(deepseek.history.spans), deepseek.components)
	}

	google, err := parseGoogleCloud(catalog["google"], file("google_incidents.json"), readAt)
	if err != nil {
		t.Fatal(err)
	}
	if google.health != HealthOperational || len(google.history.spans) != 1 {
		t.Errorf("google: health %q, %d spans", google.health, len(google.history.spans))
	}

	aws, err := parseAWSHealth(catalog["bedrock"], file("aws_currentevents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if aws.health != HealthOperational || aws.incident != nil || aws.history.known {
		t.Errorf("aws: health %q, incident %+v, history %v", aws.health, aws.incident, aws.history.known)
	}
}
