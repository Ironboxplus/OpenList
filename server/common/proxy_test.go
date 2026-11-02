package common

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

func TestProxyOverridesUpstreamContentDisposition(t *testing.T) {
	previousConfig := conf.Conf
	conf.Conf = conf.DefaultConfig("data")
	t.Cleanup(func() {
		conf.Conf = previousConfig
	})

	const content = "archive content"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="download"`)
		w.Header().Set("Content-Type", "application/x-rar-compressed")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, content)
	}))
	t.Cleanup(upstream.Close)

	file := &model.Object{
		Name: "测试文件.rar",
		Size: int64(len(content)),
	}
	link := &model.Link{URL: upstream.URL}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/sd/example", nil)

	err := Proxy(recorder, request, link, file)
	if err != nil {
		t.Fatalf("Proxy() error = %v", err)
	}
	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("status code = %d, want %d", got, want)
	}
	if got, want := recorder.Header().Get("Content-Disposition"), utils.GenerateContentDisposition(file.GetName()); got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
	if got, want := recorder.Header().Get("Content-Type"), "application/x-rar-compressed"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := recorder.Body.String(), content; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func serveUpstream(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

// AList fd126c80 (GHSA-fq52-hpvg-r5qv): a proxied html/svg must never render
// inline in our origin, whatever ?type= asks for. Here the disposition is always
// attachment, and nosniff keeps the browser from reinterpreting the type.
func TestProxyNeverServesActiveContentInline(t *testing.T) {
	previous := conf.Conf
	conf.Conf = conf.DefaultConfig("data")
	t.Cleanup(func() { conf.Conf = previous })

	for _, ct := range []string{"text/html", "text/html; charset=utf-8", "application/xhtml+xml", "image/svg+xml"} {
		up := serveUpstream(t, ct, "<script>alert(1)</script>")
		for _, query := range []string{"", "?type=preview"} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/d/evil.html"+query, nil)
			if err := Proxy(rec, req, &model.Link{URL: up.URL}, &model.Object{Name: "evil.html"}); err != nil {
				t.Fatal(err)
			}
			if d := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(d, "attachment") {
				t.Errorf("%s%s: Content-Disposition %q, want attachment", ct, query, d)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("%s%s: X-Content-Type-Options %q", ct, query, got)
			}
		}
	}
}

// The upstream type is kept (a thumbnail of a document is image/webp), which is
// what made AList stop deriving the type from the file name.
func TestProxyKeepsUpstreamContentType(t *testing.T) {
	previous := conf.Conf
	conf.Conf = conf.DefaultConfig("data")
	t.Cleanup(func() { conf.Conf = previous })

	up := serveUpstream(t, "image/webp", "RIFFxxxxWEBP")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/d/doc.pdf?type=preview", nil)
	if err := Proxy(rec, req, &model.Link{URL: up.URL}, &model.Object{Name: "doc.pdf"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/webp" {
		t.Errorf("Content-Type %q, want image/webp", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff missing")
	}
}

func TestProxyRangeReaderPathIsNosniffAttachment(t *testing.T) {
	previous := conf.Conf
	conf.Conf = conf.DefaultConfig("data")
	t.Cleanup(func() { conf.Conf = previous })

	link := &model.Link{ContentLength: 5, RangeReader: stream.RangeReaderFunc(func(_ context.Context, r http_range.Range) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("hello")), nil
	})}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/d/x.html?type=preview", nil)
	if err := Proxy(rec, req, link, &model.Object{Name: "x.html", Size: 5}); err != nil {
		t.Fatal(err)
	}
	if d := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(d, "attachment") {
		t.Errorf("Content-Disposition %q", d)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff missing")
	}
}
