package provider

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// ---- Flashduty（DeepSeek）：RSS，一條是一次事故，帶最新的狀態與受影響的組件 ----

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
}

var (
	rssStatusRE    = regexp.MustCompile(`<strong>Status:</strong>\s*([a-z_]+)`)
	rssAffectedRE  = regexp.MustCompile(`<strong>Affected components:</strong>\s*([^<]*)`)
	rssParagraphRE = regexp.MustCompile(`(?s)<p>(.*?)</p>`)
	tagRE          = regexp.MustCompile(`<[^>]+>`)
)

// parseFlashduty：RSS 沒有起止時間，只知道哪一天有事（PS16）；最新一條算進來的
// 還沒 resolved 時為 degraded（PS08）。
func parseFlashduty(v Vendor, feed []byte, now time.Time) (reading, error) {
	var doc struct {
		Channel *struct {
			Items []rssItem `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(feed, &doc); err != nil {
		return reading{}, fmt.Errorf("rss: %w", err)
	}
	if doc.Channel == nil {
		return reading{}, fmt.Errorf("rss: no channel")
	}
	r := reading{health: HealthOperational, history: history{known: true}}
	var latest *rssItem
	var latestAt time.Time
	for i, it := range doc.Channel.Items {
		at, err := time.Parse(time.RFC1123Z, strings.TrimSpace(it.PubDate))
		if err != nil {
			return reading{}, fmt.Errorf("rss: item %q: %w", it.Title, err)
		}
		if r.history.since.IsZero() || at.Before(r.history.since) {
			r.history.since = at
		}
		affected := rssAffected(it.Description)
		counted := false
		for _, c := range affected {
			if v.Track == nil || v.Track(c) {
				counted = true
				if !slices.Contains(r.components, c) {
					r.components = append(r.components, c)
				}
			}
		}
		if !counted {
			continue
		}
		open := rssStatus(it.Description) != "resolved"
		if open {
			r.health = HealthDegraded
		}
		r.history.spans = append(r.history.spans, span{from: at, to: at, health: HealthDegraded, noUptime: true})
		if (open || now.Sub(at) <= recentWindow) && (latest == nil || at.After(latestAt)) {
			latest, latestAt = &doc.Channel.Items[i], at
		}
	}
	if len(r.components) == 0 {
		r.components = v.Label
	}
	if latest != nil {
		stage := stageOf(rssStatus(latest.Description), StageInvestigating)
		r.incident = &ProviderIncident{ID: cmp.Or(latest.GUID, latest.Link), Title: strings.TrimSpace(latest.Title), URL: latest.Link,
			Updates: []IncidentUpdate{{Stage: stage, At: latestAt, Message: rssMessage(latest.Description)}}}
	}
	return r, nil
}

func rssStatus(desc string) string {
	if m := rssStatusRE.FindStringSubmatch(desc); m != nil {
		return m[1]
	}
	return ""
}

func rssAffected(desc string) []string {
	m := rssAffectedRE.FindStringSubmatch(desc)
	if m == nil {
		return nil
	}
	var out []string
	for c := range strings.SplitSeq(html.UnescapeString(m[1]), ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// rssMessage 是說明裡狀態與受影響組件之外的段落。
func rssMessage(desc string) string {
	var parts []string
	for _, m := range rssParagraphRE.FindAllStringSubmatch(desc, -1) {
		if strings.Contains(m[1], "<strong>") {
			continue
		}
		if t := strings.TrimSpace(html.UnescapeString(tagRE.ReplaceAllString(m[1], ""))); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ---- Google Cloud：incidents.json，帶起止時間、嚴重程度與受影響的產品 ----

type gcProduct struct {
	Title string `json:"title"`
}

type gcIncident struct {
	ID       string      `json:"id"`
	Begin    string      `json:"begin"`
	End      string      `json:"end"`
	Severity string      `json:"severity"`
	Desc     string      `json:"external_desc"`
	URI      string      `json:"uri"`
	Products []gcProduct `json:"affected_products"`
	Updates  []struct {
		When   string `json:"when"`
		Text   string `json:"text"`
		Status string `json:"status"`
	} `json:"updates"`
}

// googleHealth：high 是中斷，medium 算 30%，low 只標 degraded（PS09）。
func googleHealth(severity string) (Health, float64) {
	switch severity {
	case "high":
		return HealthOutage, weightOutage
	case "medium":
		return HealthDegraded, weightPartial
	}
	return HealthDegraded, weightNone
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

func parseGoogleCloud(v Vendor, feed []byte, now time.Time) (reading, error) {
	var list []gcIncident
	if err := json.Unmarshal(feed, &list); err != nil {
		return reading{}, fmt.Errorf("incidents.json: %w", err)
	}
	r := reading{health: HealthOperational, components: v.Label, history: history{known: true}}
	var latest *gcIncident
	var latestBegin, latestEnd time.Time
	for i, in := range list {
		begin, err := parseTime(in.Begin)
		if err != nil || begin.IsZero() {
			return reading{}, fmt.Errorf("incident %s: begin %q", in.ID, in.Begin)
		}
		end, err := parseTime(in.End)
		if err != nil {
			return reading{}, fmt.Errorf("incident %s: end %q", in.ID, in.End)
		}
		if r.history.since.IsZero() || begin.Before(r.history.since) {
			r.history.since = begin
		}
		if !slices.ContainsFunc(in.Products, func(p gcProduct) bool { return v.Track == nil || v.Track(p.Title) }) {
			continue
		}
		h, w := googleHealth(in.Severity)
		to := end
		if to.IsZero() {
			to = now
			r.health = worse(r.health, h)
		}
		r.history.spans = append(r.history.spans, span{from: begin, to: to, weight: w, health: h})
		if shown(end, now) && (latest == nil || begin.After(latestBegin)) {
			latest, latestBegin, latestEnd = &list[i], begin, end
		}
	}
	if latest != nil {
		out := &ProviderIncident{ID: latest.ID, Title: strings.TrimSpace(latest.Desc), URL: strings.TrimRight(v.Page, "/") + "/" + strings.TrimLeft(latest.URI, "/")}
		for _, u := range latest.Updates {
			at, err := parseTime(u.When)
			if err != nil {
				return reading{}, fmt.Errorf("incident %s: update time %q", latest.ID, u.When)
			}
			stage := StageInvestigating
			if u.Status == "AVAILABLE" || (!latestEnd.IsZero() && at.Equal(latestEnd)) {
				stage = StageResolved
			}
			out.Updates = append(out.Updates, IncidentUpdate{Stage: stage, At: at, Message: strings.TrimSpace(u.Text)})
		}
		slices.SortStableFunc(out.Updates, func(a, b IncidentUpdate) int { return b.At.Compare(a.At) })
		r.incident = out
	}
	return r, nil
}

// ---- AWS Health：當前事件，UTF-16 的 JSON，沒有歷史（PS10、PS17） ----

type awsEvent struct {
	Date        string `json:"date"`
	ARN         string `json:"arn"`
	Status      string `json:"status"` // "0" 是已解決
	Service     string `json:"service"`
	ServiceName string `json:"service_name"`
	Summary     string `json:"summary"`
	Log         []struct {
		Message   string      `json:"message"`
		Timestamp json.Number `json:"timestamp"`
	} `json:"event_log"`
}

// decodeUTF16 把帶 BOM 的 UTF-16 轉成 UTF-8；沒有 BOM 的原樣返回。
func decodeUTF16(b []byte) ([]byte, error) {
	var order binary.ByteOrder
	switch {
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		order = binary.BigEndian
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		order = binary.LittleEndian
	default:
		return b, nil
	}
	b = b[2:]
	if len(b)%2 != 0 {
		return nil, fmt.Errorf("odd UTF-16 length")
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = order.Uint16(b[2*i:])
	}
	return []byte(string(utf16.Decode(units))), nil
}

func unixTime(s string) (time.Time, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(n, 0), nil
}

func parseAWSHealth(v Vendor, feed []byte) (reading, error) {
	text, err := decodeUTF16(feed)
	if err != nil {
		return reading{}, fmt.Errorf("currentevents: %w", err)
	}
	var events []awsEvent
	if err := json.Unmarshal(text, &events); err != nil {
		return reading{}, fmt.Errorf("currentevents: %w", err)
	}
	r := reading{health: HealthOperational, components: v.Label}
	var latest *awsEvent
	var latestAt time.Time
	for i, e := range events {
		if e.Status == "0" || (v.Track != nil && !v.Track(e.Service)) {
			continue
		}
		at, err := unixTime(e.Date)
		if err != nil {
			return reading{}, fmt.Errorf("event %s: date %q", e.ARN, e.Date)
		}
		r.health = HealthDegraded
		if latest == nil || at.After(latestAt) {
			latest, latestAt = &events[i], at
		}
	}
	if latest != nil {
		out := &ProviderIncident{ID: latest.ARN, Title: strings.TrimSpace(latest.Summary), URL: v.Page}
		for _, l := range latest.Log {
			at, err := unixTime(l.Timestamp.String())
			if err != nil {
				return reading{}, fmt.Errorf("event %s: log time %q", latest.ARN, l.Timestamp)
			}
			out.Updates = append(out.Updates, IncidentUpdate{Stage: StageInvestigating, At: at, Message: strings.TrimSpace(l.Message)})
		}
		slices.SortStableFunc(out.Updates, func(a, b IncidentUpdate) int { return b.At.Compare(a.At) })
		r.incident = out
	}
	return r, nil
}
