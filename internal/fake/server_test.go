package fake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestServerHeadersAndStats(t *testing.T) {
	l, _ := newTestLimiter(t, Config{Budget: 2, Window: 60 * time.Second, MaxConcurrent: 10, Points: 100, PointsWindow: time.Second})
	srv := httptest.NewServer(NewServer(l, 0))
	defer srv.Close()

	get := func() *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/repos/o/r/pulls", nil)
		req.Header.Set("Authorization", "token abc")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	resp := get()
	wantReset := "1792411260" // 2026-10-19 12:01:00 UTC
	h := resp.Header
	if resp.StatusCode != 200 || h.Get("x-ratelimit-limit") != "2" || h.Get("x-ratelimit-remaining") != "1" ||
		h.Get("x-ratelimit-used") != "1" || h.Get("x-ratelimit-reset") != wantReset || h.Get("x-ratelimit-resource") != "core" {
		t.Fatalf("first response: %d %v", resp.StatusCode, h)
	}
	get()
	if resp := get(); resp.StatusCode != http.StatusForbidden || resp.Header.Get("x-ratelimit-remaining") != "0" {
		t.Fatalf("over budget: %d remaining %q; want 403, 0", resp.StatusCode, resp.Header.Get("x-ratelimit-remaining"))
	}

	statsResp, err := http.Get(srv.URL + "/__stats")
	if err != nil {
		t.Fatal(err)
	}
	defer statsResp.Body.Close()
	var stats map[string]Stats
	if err := json.NewDecoder(statsResp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	want := Stats{Requests: 3, OK: 2, Primary403: 1, Violations: 1, Windows: 1, WindowReset: 1792411260}
	if got := stats["token abc"]; got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
}

func TestServerRetryAfterHeader(t *testing.T) {
	l, _ := newTestLimiter(t, Config{Budget: 10, Window: 60 * time.Second, MaxConcurrent: 10, Points: 5, PointsWindow: time.Second})
	srv := httptest.NewServer(NewServer(l, 0))
	defer srv.Close()

	post := func() *http.Response {
		resp, err := http.Post(srv.URL+"/repos/o/r/issues/1/comments", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := post(); resp.StatusCode != 200 {
		t.Fatalf("first write: %d", resp.StatusCode)
	}
	resp := post()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("retry-after") != "1" {
		t.Fatalf("second write: %d retry-after %q; want 429, 1", resp.StatusCode, resp.Header.Get("retry-after"))
	}
}
