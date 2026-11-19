package provider

import (
	"math"
	"slices"
	"time"
)

// dayCount 是色條的天數。
const dayCount = 30

// recentWindow：解決後還在一行裡展開的時間（PS07）。
const recentWindow = 24 * time.Hour

// 停機的權重（PS11、PS12）：完全中斷全算，部分中斷算 30%，性能下降不算。
const (
	weightOutage  = 1.0
	weightPartial = 0.3
	weightNone    = 0.0
)

// span 是一段不正常的時間：[from, to)，按 weight 算進停機，這段時間的格子
// 至少是 health。noUptime：只知道哪一刻有事、不知道多久（Flashduty 的 RSS），
// 這一天標上 health、在線率為空（PS16）；這種 span 的 from 等於 to。
type span struct {
	from, to time.Time
	weight   float64
	health   Health
	noUptime bool
}

// history 是一家讀到的歷史，還沒按天算。
type history struct {
	// known：有歷史可算；false 時 30 天都是無數據（PS17）。
	known bool
	// since 是事故列表最早一條的開始；更早的日子無數據（PS13）。complete 時不管它。
	since time.Time
	// complete：事故列表沒被截斷，30 天都有數據。
	complete bool
	spans    []span
}

// days 把歷史按 zone 的日曆日切成最近 30 天，舊的在前；今天只算到 now（PS15）。
// 跨過午夜的 span 兩天各算各自那一段（PS14）。
func days(h history, now time.Time, zone *time.Location) []ProviderDay {
	local := now.In(zone)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	out := make([]ProviderDay, 0, dayCount)
	for i := dayCount - 1; i >= 0; i-- {
		start := today.AddDate(0, 0, -i)
		end := start.AddDate(0, 0, 1)
		if end.After(now) {
			end = now
		}
		d := ProviderDay{Date: start.Format(time.DateOnly)}
		if h.known && (h.complete || end.After(h.since)) {
			d.Uptime, d.Health = day(h.spans, start, end)
		}
		out = append(out, d)
	}
	return out
}

// day 算 [start, end) 這一天：最差的狀態，與扣掉加權停機後的在線率。
func day(spans []span, start, end time.Time) (*float64, Health) {
	health := HealthOperational
	var in []span
	unknown := false
	for _, s := range spans {
		if s.noUptime {
			if !s.from.Before(start) && s.from.Before(end) {
				health, unknown = worse(health, s.health), true
			}
			continue
		}
		if s.from.Before(end) && s.to.After(start) {
			health = worse(health, s.health)
			in = append(in, span{from: maxTime(s.from, start), to: minTime(s.to, end), weight: s.weight})
		}
	}
	if unknown {
		return nil, health
	}
	length := end.Sub(start)
	if length <= 0 {
		up := 100.0
		return &up, health
	}
	up := 100 * (1 - downtime(in, start, end).Seconds()/length.Seconds())
	up = math.Round(up*1000) / 1000
	return &up, health
}

// downtime 是加權的停機時間：重疊的部分取權重最大的那一段，不重複算。
func downtime(in []span, start, end time.Time) time.Duration {
	points := []time.Time{start, end}
	for _, s := range in {
		points = append(points, s.from, s.to)
	}
	slices.SortFunc(points, func(a, b time.Time) int { return a.Compare(b) })
	var total float64
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if !b.After(a) {
			continue
		}
		w := 0.0
		for _, s := range in {
			if !s.from.After(a) && !s.to.Before(b) {
				w = max(w, s.weight)
			}
		}
		total += w * float64(b.Sub(a))
	}
	return time.Duration(total)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
