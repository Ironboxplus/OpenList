package _115_open

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"golang.org/x/time/rate"
)

type recordedRequest struct {
	Path string
	Form url.Values
	Time time.Time
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
			Time: time.Now(),
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

// --- FlexString / numeric CID tests ---

func TestFlexStringUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"string value", `{"cid":"dir-1"}`, "dir-1"},
		{"integer value", `{"cid":3383942108160578280}`, "3383942108160578280"},
		{"large integer", `{"cid":9999999999999999999}`, "9999999999999999999"},
		{"zero", `{"cid":0}`, "0"},
		{"negative", `{"cid":-123}`, "-123"},
		{"float", `{"cid":1.5}`, "1.5"},
		{"empty string", `{"cid":""}`, ""},
		{"null", `{"cid":null}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v struct {
				CID sdk.FlexString `json:"cid"`
			}
			if err := json.Unmarshal([]byte(tt.input), &v); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if got := string(v.CID); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFlexStringUnmarshalInvalid(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"boolean", `{"cid":true}`},
		{"array", `{"cid":[1]}`},
		{"object", `{"cid":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v struct {
				CID sdk.FlexString `json:"cid"`
			}
			if err := json.Unmarshal([]byte(tt.input), &v); err == nil {
				t.Fatalf("expected error for input %s", tt.input)
			}
		})
	}
}

func TestRbListRespUnmarshalCIDAsString(t *testing.T) {
	raw := `{"id":"rb-1","file_name":"demo.txt","cid":"dir-1","file_size":"123"}`
	var info sdk.RbListResp_FileInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if string(info.CID) != "dir-1" {
		t.Fatalf("got CID %q, want %q", string(info.CID), "dir-1")
	}
}

func TestRbListRespUnmarshalCIDAsNumber(t *testing.T) {
	raw := `{"id":"rb-1","file_name":"MyFolder","cid":3383942108160578280,"file_size":"0"}`
	var info sdk.RbListResp_FileInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if string(info.CID) != "3383942108160578280" {
		t.Fatalf("got CID %q, want %q", string(info.CID), "3383942108160578280")
	}
}

// --- matchRecycleBinEntry with numeric CID ---

func TestMatchRecycleBinEntryDirMatchWithNumericCID(t *testing.T) {
	// obj represents a directory with Pid (parent ID) as a large number
	obj := &Obj{Fid: "folder-1", Pid: "3383942108160578280", Fn: "MyFolder", Fc: "0", FS: 0}
	files := map[string]sdk.RbListResp_FileInfo{
		"rb-1": {
			ID:       "rb-folder-1",
			FileName: "MyFolder",
			CID:      sdk.FlexString("3383942108160578280"),
		},
	}
	result := matchRecycleBinEntry(obj, files)
	if result == nil {
		t.Fatal("expected match for directory with numeric CID, got nil")
	}
	if result.ID != "rb-folder-1" {
		t.Fatalf("got ID %q, want %q", result.ID, "rb-folder-1")
	}
}

func TestMatchRecycleBinEntryDirNoMatchWhenCIDWrong(t *testing.T) {
	obj := &Obj{Fid: "folder-1", Pid: "3383942108160578280", Fn: "MyFolder", Fc: "0", FS: 0}
	files := map[string]sdk.RbListResp_FileInfo{
		"rb-1": {
			ID:       "rb-folder-1",
			FileName: "MyFolder",
			CID:      sdk.FlexString("9999999999"),
		},
	}
	result := matchRecycleBinEntry(obj, files)
	if result != nil {
		t.Fatalf("expected no match, got %+v", result)
	}
}

func TestMatchRecycleBinEntryFileSHA1MatchWithNumericCID(t *testing.T) {
	obj := &Obj{Fid: "file-1", Pid: "3383942108160578280", Fn: "video.mp4", Fc: "1", FS: 1024, Sha1: "abc123"}
	files := map[string]sdk.RbListResp_FileInfo{
		"rb-1": {
			ID:       "rb-file-1",
			FileName: "video.mp4",
			CID:      sdk.FlexString("3383942108160578280"),
			SHA1:     "ABC123",
			FileSize: "1024",
		},
	}
	result := matchRecycleBinEntry(obj, files)
	if result == nil {
		t.Fatal("expected match via SHA1+CID, got nil")
	}
	if result.ID != "rb-file-1" {
		t.Fatalf("got ID %q, want %q", result.ID, "rb-file-1")
	}
}

func TestMatchRecycleBinEntryFileSHA1MatchByNameOnly(t *testing.T) {
	obj := &Obj{Fid: "file-1", Pid: "wrong-pid", Fn: "video.mp4", Fc: "1", FS: 1024, Sha1: "abc123"}
	files := map[string]sdk.RbListResp_FileInfo{
		"rb-1": {
			ID:       "rb-file-1",
			FileName: "video.mp4",
			CID:      sdk.FlexString("3383942108160578280"),
			SHA1:     "ABC123",
			FileSize: "1024",
		},
	}
	result := matchRecycleBinEntry(obj, files)
	if result == nil {
		t.Fatal("expected match via SHA1+name, got nil")
	}
}

func TestMatchRecycleBinEntryFileNameSizeCIDMatch(t *testing.T) {
	obj := &Obj{Fid: "file-1", Pid: "3383942108160578280", Fn: "doc.pdf", Fc: "1", FS: 500}
	files := map[string]sdk.RbListResp_FileInfo{
		"rb-1": {
			ID:       "rb-file-1",
			FileName: "doc.pdf",
			CID:      sdk.FlexString("3383942108160578280"),
			FileSize: "500",
		},
	}
	result := matchRecycleBinEntry(obj, files)
	if result == nil {
		t.Fatal("expected match via name+size+CID, got nil")
	}
}

func TestMatchRecycleBinEntryDirectIDMatch(t *testing.T) {
	obj := &Obj{Fid: "file-1", Pid: "dir-1", Fn: "demo.txt", Fc: "1", FS: 123}
	files := map[string]sdk.RbListResp_FileInfo{
		"file-1": {
			ID:       "rb-123",
			FileName: "demo.txt",
			CID:      sdk.FlexString("dir-1"),
		},
	}
	result := matchRecycleBinEntry(obj, files)
	if result == nil {
		t.Fatal("expected direct ID match, got nil")
	}
	if result.ID != "rb-123" {
		t.Fatalf("got ID %q, want %q", result.ID, "rb-123")
	}
}

func TestMatchRecycleBinEntryEmptyFiles(t *testing.T) {
	obj := &Obj{Fid: "file-1", Pid: "dir-1", Fn: "demo.txt", Fc: "1", FS: 123}
	result := matchRecycleBinEntry(obj, nil)
	if result != nil {
		t.Fatalf("expected nil for nil files, got %+v", result)
	}
	result = matchRecycleBinEntry(obj, map[string]sdk.RbListResp_FileInfo{})
	if result != nil {
		t.Fatalf("expected nil for empty files, got %+v", result)
	}
}

// --- Full Remove flow with numeric CID in recycle bin ---

func TestOpen115RemoveDeleteWithNumericCID(t *testing.T) {
	driver, requests := newTestOpen115(t, "delete", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/ufile/delete":
			writeSDKSuccess(t, w, []string{"folder-1"})
		case "/open/rb/del":
			if r.FormValue("tid") == "folder-1" {
				writeSDKError(t, w, 404, "not found")
				return
			}
			writeSDKSuccess(t, w, []string{"rb-folder-1"})
		case "/open/rb/list":
			// CID returned as number (the real bug scenario)
			writeSDKSuccess(t, w, map[string]any{
				"offset":  0,
				"limit":   1,
				"count":   "1",
				"rb_pass": 0,
				"rb-folder-1": map[string]any{
					"id":        "rb-folder-1",
					"file_name": "MyFolder",
					"cid":       3383942108160578280,
					"file_size": "0",
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	obj := &Obj{Fid: "folder-1", Pid: "3383942108160578280", Fn: "MyFolder", Fc: "0", FS: 0}
	if err := driver.Remove(context.Background(), obj); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}

	assertRequestPaths(t, requests(), "/open/ufile/delete", "/open/rb/del", "/open/rb/list", "/open/rb/del")
	assertFormValue(t, requests()[3].Form, "tid", "rb-folder-1")
}

func TestOpen115RemoveDeleteWithStringCIDStillWorks(t *testing.T) {
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
			// CID returned as string (normal case)
			writeSDKSuccess(t, w, map[string]any{
				"offset":  0,
				"limit":   1,
				"count":   "1",
				"rb_pass": 0,
				"rb-123": map[string]any{
					"id":        "rb-123",
					"file_name": "demo.txt",
					"cid":       "dir-1",
					"sha1":      "sha-demo",
					"file_size": "123",
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	obj := &Obj{Fid: "file-1", Pid: "dir-1", Fn: "demo.txt", Fc: "1", FS: 123, Sha1: "sha-demo"}
	if err := driver.Remove(context.Background(), obj); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}

	assertRequestPaths(t, requests(), "/open/ufile/delete", "/open/rb/del", "/open/rb/list", "/open/rb/del")
	assertFormValue(t, requests()[3].Form, "tid", "rb-123")
}

func TestOpen115RemoveDeleteRetriesRecycleBinLookupUntilVisible(t *testing.T) {
	oldAttempts, oldDelay := recycleBinLookupMaxAttempts, recycleBinLookupRetryDelay
	recycleBinLookupMaxAttempts = 3
	recycleBinLookupRetryDelay = time.Millisecond
	t.Cleanup(func() {
		recycleBinLookupMaxAttempts = oldAttempts
		recycleBinLookupRetryDelay = oldDelay
	})

	rbListCalls := 0
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
			rbListCalls++
			if rbListCalls < 3 {
				writeSDKSuccess(t, w, map[string]any{
					"offset":  0,
					"limit":   1,
					"count":   "0",
					"rb_pass": 0,
				})
				return
			}
			writeSDKSuccess(t, w, map[string]any{
				"offset":  0,
				"limit":   1,
				"count":   "1",
				"rb_pass": 0,
				"rb-123": map[string]any{
					"id":        "rb-123",
					"file_name": "demo.txt",
					"cid":       "dir-1",
					"sha1":      "sha-demo",
					"file_size": "123",
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	obj := &Obj{Fid: "file-1", Pid: "dir-1", Fn: "demo.txt", Fc: "1", FS: 123, Sha1: "sha-demo"}
	if err := driver.Remove(context.Background(), obj); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}

	if rbListCalls != 3 {
		t.Fatalf("rbListCalls = %d, want 3", rbListCalls)
	}
	assertRequestPaths(t, requests(), "/open/ufile/delete", "/open/rb/del", "/open/rb/list", "/open/rb/list", "/open/rb/list", "/open/rb/del")
	assertFormValue(t, requests()[5].Form, "tid", "rb-123")
}

func TestOpen115RemoveDeleteStopsRetryWhenContextCancelled(t *testing.T) {
	oldAttempts, oldDelay := recycleBinLookupMaxAttempts, recycleBinLookupRetryDelay
	recycleBinLookupMaxAttempts = 5
	recycleBinLookupRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() {
		recycleBinLookupMaxAttempts = oldAttempts
		recycleBinLookupRetryDelay = oldDelay
	})

	driver, _ := newTestOpen115(t, "delete", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/ufile/delete":
			writeSDKSuccess(t, w, []string{"file-1"})
		case "/open/rb/del":
			writeSDKError(t, w, 404, "not found")
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	obj := &Obj{Fid: "file-1", Pid: "dir-1", Fn: "demo.txt", Fc: "1", FS: 123, Sha1: "sha-demo"}
	err := driver.Remove(ctx, obj)
	if err == nil {
		t.Fatalf("expected Remove to fail due to context cancellation")
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("expected context deadline exceeded, got: %v", err)
	}
}

// --- Put rate-limiting tests ---

// mockFileStreamer satisfies model.FileStreamer with pre-computed hashes for testing.
type mockFileStreamer struct {
	name     string
	size     int64
	hashInfo utils.HashInfo
	data     []byte
}

func (m *mockFileStreamer) Read(p []byte) (int, error)                    { return 0, io.EOF }
func (m *mockFileStreamer) Close() error                                  { return nil }
func (m *mockFileStreamer) Add(_ io.Closer)                               {}
func (m *mockFileStreamer) AddIfCloser(_ any)                             {}
func (m *mockFileStreamer) GetSize() int64                                { return m.size }
func (m *mockFileStreamer) GetName() string                               { return m.name }
func (m *mockFileStreamer) ModTime() time.Time                            { return time.Time{} }
func (m *mockFileStreamer) CreateTime() time.Time                         { return time.Time{} }
func (m *mockFileStreamer) IsDir() bool                                   { return false }
func (m *mockFileStreamer) GetHash() utils.HashInfo                       { return m.hashInfo }
func (m *mockFileStreamer) GetID() string                                 { return "" }
func (m *mockFileStreamer) GetPath() string                               { return "" }
func (m *mockFileStreamer) GetMimetype() string                           { return "application/octet-stream" }
func (m *mockFileStreamer) NeedStore() bool                               { return false }
func (m *mockFileStreamer) IsForceStreamUpload() bool                     { return false }
func (m *mockFileStreamer) GetExist() model.Obj                           { return nil }
func (m *mockFileStreamer) SetExist(_ model.Obj)                          {}
func (m *mockFileStreamer) GetFile() model.File                           { return nil }
func (m *mockFileStreamer) RangeRead(_ http_range.Range) (io.Reader, error) {
	return strings.NewReader(string(m.data)), nil
}
func (m *mockFileStreamer) CacheFullAndWriter(_ *model.UpdateProgress, _ io.Writer) (model.File, error) {
	return nil, nil
}

func newTestOpen115WithRateLimit(t *testing.T, limitRate float64, responder http.HandlerFunc) (*Open115, func() []recordedRequest) {
	t.Helper()
	driver, requests := newTestOpen115(t, "trash", responder)
	driver.limiter = rate.NewLimiter(rate.Limit(limitRate), 1)
	return driver, requests
}

func TestPutRateLimitsEverySDKCall(t *testing.T) {
	// Rate limit: 10 req/s → each WaitLimit blocks ~100ms
	const limitRate = 10.0

	driver, requests := newTestOpen115WithRateLimit(t, limitRate, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/upload/init":
			// First call: status=1 (not rapid), second call: status=2 (rapid success)
			if r.FormValue("sign_key") != "" {
				writeSDKSuccess(t, w, map[string]any{"status": 2})
			} else {
				writeSDKSuccess(t, w, map[string]any{
					"status":     7,
					"sign_key":   "test-key",
					"sign_check": "0-10",
				})
			}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	stream := &mockFileStreamer{
		name: "test.txt",
		size: 100,
		hashInfo: utils.NewHashInfoByMap(map[*utils.HashType]string{
			utils.SHA1:      "da39a3ee5e6b4b0d3255bfef95601890afd80709",
			utils.SHA1_128K: "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		}),
		data: make([]byte, 100),
	}
	dstDir := &model.Object{ID: "0", Name: "root", IsFolder: true}
	up := func(float64) {}

	start := time.Now()
	err := driver.Put(context.Background(), dstDir, stream, up)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	reqs := requests()
	// Expect: pre-hash UploadInit + main UploadInit + sign-check UploadInit = 3 calls
	if len(reqs) < 3 {
		t.Fatalf("expected at least 3 requests, got %d", len(reqs))
	}
	assertRequestPaths(t, reqs, "/open/upload/init", "/open/upload/init", "/open/upload/init")

	// With 2 SDK calls each preceded by WaitLimit(10/s), minimum elapsed is ~100ms.
	// Without WaitLimit, both calls fire instantly (<10ms).
	minExpected := time.Duration(float64(time.Second) / limitRate * float64(len(reqs)-1))
	tolerance := minExpected * 7 / 10 // 70% to account for timing jitter
	if elapsed < tolerance {
		t.Fatalf("Put completed too fast (%v), expected at least %v — WaitLimit likely missing before some SDK calls", elapsed, tolerance)
	}

	// Also verify individual request spacing
	for i := 1; i < len(reqs); i++ {
		gap := reqs[i].Time.Sub(reqs[i-1].Time)
		gapMin := time.Duration(float64(time.Second) / limitRate * 0.7)
		if gap < gapMin {
			t.Fatalf("gap between request %d and %d is %v, expected at least %v — WaitLimit missing", i-1, i, gap, gapMin)
		}
	}
}

func TestPutRateLimitsPreHashPath(t *testing.T) {
	const limitRate = 10.0

	driver, requests := newTestOpen115WithRateLimit(t, limitRate, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/upload/init":
			// Rapid upload success on first try
			writeSDKSuccess(t, w, map[string]any{"status": 2})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	stream := &mockFileStreamer{
		name: "test.txt",
		size: 100,
		hashInfo: utils.NewHashInfoByMap(map[*utils.HashType]string{
			utils.SHA1:      "da39a3ee5e6b4b0d3255bfef95601890afd80709",
			utils.SHA1_128K: "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		}),
		data: make([]byte, 100),
	}
	dstDir := &model.Object{ID: "0", Name: "root", IsFolder: true}

	err := driver.Put(context.Background(), dstDir, stream, func(float64) {})
	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d: %v", len(reqs), reqs)
	}
	assertRequestPaths(t, reqs, "/open/upload/init")
}
