package _115_open

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type recordedRequest struct {
	Path string
	Form url.Values
}

type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Scheme = t.target.Scheme
	cloned.URL.Host = t.target.Host
	return t.base.RoundTrip(cloned)
}

func TestOpen115RemoveTrashUsesDelFileOnly(t *testing.T) {
	driver, requests := newTestOpen115(t, "trash", func(w http.ResponseWriter, r *http.Request) {
		writeSDKSuccess(t, w, []string{"rb-123"})
	})

	obj := &Obj{Fid: "file-1", Pid: "dir-1", Fn: "demo.txt", FS: 123, Sha1: "sha-demo"}
	if err := driver.Remove(context.Background(), obj); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}

	assertRequestPaths(t, requests(), "/open/ufile/delete")
	assertFormValue(t, requests()[0].Form, "file_ids", "file-1")
	assertFormValue(t, requests()[0].Form, "parent_id", "dir-1")
}

func TestOpen115RemoveDeleteUsesDelFileResponseIDWhenAvailable(t *testing.T) {
	driver, requests := newTestOpen115(t, "delete", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/ufile/delete":
			writeSDKSuccess(t, w, []string{"rb-123"})
		case "/open/rb/del":
			writeSDKSuccess(t, w, []string{"rb-123"})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	obj := &Obj{Fid: "file-1", Pid: "dir-1", Fn: "demo.txt", FS: 123, Sha1: "sha-demo"}
	if err := driver.Remove(context.Background(), obj); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}

	assertRequestPaths(t, requests(), "/open/ufile/delete", "/open/rb/del")
	assertFormValue(t, requests()[1].Form, "tid", "rb-123")
}

func TestOpen115RemoveDeleteFallsBackToRecycleBinLookup(t *testing.T) {
	driver, requests := newTestOpen115(t, "delete", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/ufile/delete":
			writeSDKSuccess(t, w, []string{"file-1"})
		case "/open/rb/del":
			if r.FormValue("tid") == "file-1" {
				writeSDKError(t, w, 404, "not found")
				return
			}
			writeSDKSuccess(t, w, []string{"rb-123"})
		case "/open/rb/list":
			writeSDKSuccess(t, w, map[string]any{
				"offset":  0,
				"limit":   1,
				"count":   "1",
				"rb_pass": 0,
				"rb-123": map[string]any{
					"id":        "rb-123",
					"file_name": "demo.txt",
					"file_size": "123",
					"cid":       "dir-1",
					"sha1":      "sha-demo",
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	obj := &Obj{Fid: "file-1", Pid: "dir-1", Fn: "demo.txt", FS: 123, Sha1: "sha-demo"}
	if err := driver.Remove(context.Background(), obj); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}

	assertRequestPaths(t, requests(), "/open/ufile/delete", "/open/rb/del", "/open/rb/list", "/open/rb/del")
	assertFormValue(t, requests()[3].Form, "tid", "rb-123")
}

func TestOpen115RemoveDeleteReturnsErrorWhenRecycleEntryMissing(t *testing.T) {
	driver, _ := newTestOpen115(t, "delete", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/ufile/delete":
			writeSDKSuccess(t, w, []string{})
		case "/open/rb/list":
			writeSDKSuccess(t, w, map[string]any{
				"offset":  0,
				"limit":   1,
				"count":   "0",
				"rb_pass": 0,
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	obj := &Obj{Fid: "file-1", Pid: "dir-1", Fn: "demo.txt", FS: 123, Sha1: "sha-demo"}
	err := driver.Remove(context.Background(), obj)
	if err == nil {
		t.Fatalf("expected Remove to fail when recycle-bin entry is missing")
	}
	if !strings.Contains(err.Error(), "recycle bin entry not found") {
		t.Fatalf("expected recycle-bin lookup error, got: %v", err)
	}
}

func TestOpen115DriverInfoIncludesRemoveWay(t *testing.T) {
	info, ok := op.GetDriverInfoMap()["115 Open"]
	if !ok {
		t.Fatalf("115 Open driver info was not registered")
	}

	for _, item := range info.Additional {
		if item.Name != "remove_way" {
			continue
		}
		if item.Type != "select" {
			t.Fatalf("unexpected remove_way type: %q", item.Type)
		}
		if item.Options != "trash,delete" {
			t.Fatalf("unexpected remove_way options: %q", item.Options)
		}
		if item.Default != "trash" {
			t.Fatalf("unexpected remove_way default: %q", item.Default)
		}
		if !item.Required {
			t.Fatalf("expected remove_way to be required")
		}
		return
	}

	t.Fatalf("remove_way item not found in 115 Open driver info")
}

func newTestOpen115(t *testing.T, removeWay string, responder http.HandlerFunc) (*Open115, func() []recordedRequest) {
	t.Helper()

	var (
		mu       sync.Mutex
		requests []recordedRequest
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm failed: %v", err)
		}
		mu.Lock()
		requests = append(requests, recordedRequest{
			Path: r.URL.Path,
			Form: cloneValues(r.Form),
		})
		mu.Unlock()
		responder(w, r)
	}))
	t.Cleanup(server.Close)

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse server URL failed: %v", err)
	}

	client := sdk.New(sdk.WithAccessToken("test-token"))
	client.SetHttpClient(&http.Client{
		Transport: &rewriteTransport{
			target: target,
			base:   http.DefaultTransport,
		},
	})

	return &Open115{
			Addition: Addition{
				RemoveWay: removeWay,
				PageSize:  1,
			},
			client: client,
		}, func() []recordedRequest {
			mu.Lock()
			defer mu.Unlock()
			return append([]recordedRequest(nil), requests...)
		}
}

func writeSDKSuccess(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	writeSDKResponse(t, w, map[string]any{
		"state": true,
		"data":  data,
	})
}

func writeSDKError(t *testing.T, w http.ResponseWriter, code int64, message string) {
	t.Helper()
	writeSDKResponse(t, w, map[string]any{
		"state":   false,
		"code":    code,
		"message": message,
	})
}

func writeSDKResponse(t *testing.T, w http.ResponseWriter, payload map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("Encode response failed: %v", err)
	}
}

func assertRequestPaths(t *testing.T, requests []recordedRequest, want ...string) {
	t.Helper()
	got := make([]string, 0, len(requests))
	for _, req := range requests {
		got = append(got, req.Path)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected request paths: got %v want %v", got, want)
	}
}

func assertFormValue(t *testing.T, form url.Values, key, want string) {
	t.Helper()
	if got := form.Get(key); got != want {
		t.Fatalf("unexpected form value for %s: got %q want %q", key, got, want)
	}
}

func cloneValues(src url.Values) url.Values {
	dst := make(url.Values, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}
