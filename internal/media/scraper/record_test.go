package scraper

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecorderKeepsAnswersWithoutSecretsAndStopsAtTheCap(t *testing.T) {
	var inflight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		time.Sleep(5 * time.Millisecond)
		inflight.Add(-1)
		// a misbehaving provider that echoes the key
		_, _ = w.Write([]byte(`{"results":[],"echo":"` + r.URL.Query().Get("api_key") + `"}`))
	}))
	defer srv.Close()
	cfg := Config{TMDBToken: "sekret-key-123", BaseURLs: map[string]string{"tmdb": srv.URL + "/3"}}
	rec, err := Record(&cfg, 3)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/3/search/tv?query=x&api_key=sekret-key-123", nil)
			req.Header.Set("Authorization", "Bearer sekret-key-123")
			resp, err := cfg.HTTPClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()
	}
	limited := 0
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil && strings.Contains(err.Error(), ErrRecordLimit.Error()) {
			limited++
		}
	}
	if limited != 1 || peak.Load() != 1 {
		t.Fatalf("limited=%d peak concurrency=%d, want 1 and 1", limited, peak.Load())
	}
	data, err := rec.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("sekret-key-123")) || !bytes.Contains(data, []byte("redacted")) || bytes.Contains(data, []byte("api_key")) {
		t.Fatalf("output: %s", data)
	}
	if rec.Entries() != 3 || !strings.Contains(rec.Summary(), "tmdb=3") || strings.Contains(rec.Summary(), "http") {
		t.Fatalf("summary: %s", rec.Summary())
	}
}

func TestRecorderRefusesToWriteASurvivingSecret(t *testing.T) {
	// a secret the plain replace removes entirely is fine
	r := &Recorder{secrets: []string{"abcd1234"}, entries: []RecordedResponse{{Body: "x abcd1234 y"}}}
	data, err := r.Marshal()
	if err != nil || strings.Contains(string(data), "abcd1234") {
		t.Fatalf("%s %v", data, err)
	}
	// a secret split by the replacement itself cannot be told apart, so the check is on the output
	r = &Recorder{secrets: []string{"<redacted"}, entries: []RecordedResponse{{Body: "<redac<redacted"}}}
	if _, err := r.Marshal(); err == nil {
		t.Fatal("a secret that reappears after redaction was written")
	}
}

func TestRecorderKeepsPostBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		_, _ = w.Write(append([]byte("got "), b.Bytes()...))
	}))
	defer srv.Close()
	cfg := Config{BaseURLs: map[string]string{"bgm": srv.URL}}
	rec, err := Record(&cfg, 10)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cfg.HTTPClient.Post(srv.URL+"/v0/search/subjects?limit=10", "application/json", strings.NewReader(`{"keyword":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	_, _ = got.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	if got.String() != `got {"keyword":"x"}` {
		t.Fatalf("the server saw %q", got.String())
	}
	data, _ := rec.Marshal()
	if !strings.Contains(string(data), `"method":"POST"`) || !strings.Contains(string(data), `keyword`) {
		t.Fatalf("recorded: %s", data)
	}
}
