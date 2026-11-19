package app

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/bayuncao/oriel/internal/proc"
	"github.com/bayuncao/oriel/internal/provider"
	"github.com/bayuncao/oriel/internal/usage"
)

// TestLiveProviders 經這台機器的代理設定讀各家真實的狀態頁，印出每家的狀態、組件、
// 有數據的天數與事故標題；沒讀成的印出原因。用來確認內建清單仍對得上各家的格式。
func TestLiveProviders(t *testing.T) {
	if os.Getenv("ORIELHUB_LIVE_PROVIDERS") == "" {
		t.Skip("set ORIELHUB_LIVE_PROVIDERS=1 to read the providers' public status pages")
	}
	// 與 registryHTTP 同樣按環境的代理變量走（PS28）。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(req *http.Request) (*url.URL, error) { return proxyOf(proc.Inherited())(req.URL) }
	client := &http.Client{Transport: transport}
	m := provider.New(provider.Options{HTTP: client, UserAgent: "OrielHub/live-test", Zone: usage.SystemZone})
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	b, err := m.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read %d providers in %s", len(provider.Catalog()), time.Since(start).Round(time.Millisecond))
	for _, r := range b.Rows {
		known := 0
		for _, d := range r.Days {
			if d.Uptime != nil || d.Health != "" {
				known++
			}
		}
		incident := ""
		if r.Incident != nil {
			incident = " · incident: " + r.Incident.Title
		}
		t.Logf("%-14s %-11s %2d days with data, whole page %-5v %q%s", r.Name, r.Health, known, r.WholePage, r.Components, incident)
	}
	for _, u := range b.Unread {
		t.Errorf("%s not read: %s %v", u.Name, u.Problem.Code, u.Problem.Params)
	}
}
