package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReleaseNotesURL(t *testing.T) {
	got := releaseNotesURL("Kynos-AI/OrielHub-Releases", "0.1.5")
	want := "https://github.com/Kynos-AI/OrielHub-Releases/releases/download/v0.1.5/release-notes.json"
	if got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}

func TestFetchReleaseNotes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"version":"0.1.5"}`))
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", 2<<20)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	body, err := fetchReleaseNotes(t.Context(), srv.Client(), srv.URL+"/ok")
	if err != nil || string(body) != `{"version":"0.1.5"}` {
		t.Errorf("ok: %q, %v", body, err)
	}
	if _, err := fetchReleaseNotes(t.Context(), srv.Client(), srv.URL+"/missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing: err = %v, want 404", err)
	}
	// 超過上限只讀到上限多一個位元組，交給 updater 拒絕。
	body, err = fetchReleaseNotes(t.Context(), srv.Client(), srv.URL+"/big")
	if err != nil || len(body) != 1<<20+1 {
		t.Errorf("big: %d bytes, %v", len(body), err)
	}
}
