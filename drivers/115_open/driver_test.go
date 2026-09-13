package _115_open

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	driverpkg "github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/glebarez/sqlite"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
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

func TestOpen115ShouldNotifyTokenValidForInvalidTransitionOrStaleStatus(t *testing.T) {
	driver := &Open115{}
	driver.Storage.Status = "old init error"
	if !driver.shouldNotifyTokenValid() {
		t.Fatalf("expected stale storage status to trigger token-valid notification")
	}
	if driver.tokenInvalid.Load() {
		t.Fatalf("stale-status notification should not mark tokenInvalid")
	}

	driver.Storage.Status = op.WORK
	if driver.shouldNotifyTokenValid() {
		t.Fatalf("expected healthy storage with no invalid transition to skip notification")
	}

	driver.tokenInvalid.Store(true)
	if !driver.shouldNotifyTokenValid() {
		t.Fatalf("expected invalid-to-valid transition to trigger token-valid notification")
	}
	if driver.tokenInvalid.Load() {
		t.Fatalf("expected tokenInvalid latch to be cleared")
	}
}

func TestOpen115SDKClientUsesConfiguredProxy(t *testing.T) {
	oldConf := conf.Conf
	t.Cleanup(func() {
		conf.Conf = oldConf
	})

	var gotURL string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(proxy.Close)

	conf.Conf = &conf.Config{ProxyAddress: proxy.URL}
	client := sdk.New()
	applySDKProxyIfConfigured(client)

	resp, err := client.Request(
		context.Background(),
		"http://openlist-proxy-test.invalid/ping",
		http.MethodGet,
	)
	if err != nil {
		t.Fatalf("expected request to go through configured proxy: %v", err)
	}
	if resp.StatusCode() != http.StatusOK {
		t.Fatalf("unexpected status via proxy: %d", resp.StatusCode())
	}
	if gotURL != "http://openlist-proxy-test.invalid/ping" {
		t.Fatalf("proxy did not receive the absolute target URL, got %q", gotURL)
	}
}

func TestOpen115InitRateLimitsAuthAndRootInfo(t *testing.T) {
	const limitRate = 10.0

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

		switch r.URL.Path {
		case "/open/user/info":
			writeSDKSuccess(t, w, map[string]any{})
		case "/open/folder/get_info":
			writeSDKSuccess(t, w, map[string]any{
				"file_id":   "root-1",
				"file_name": "Media",
				"paths": []map[string]string{
					{"file_id": "0", "file_name": ""},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse server URL failed: %v", err)
	}

	oldNewClient := new115SDKClient
	t.Cleanup(func() {
		new115SDKClient = oldNewClient
	})
	new115SDKClient = func(opts ...sdk.Option) *sdk.Client {
		client := sdk.New(opts...)
		client.SetHttpClient(&http.Client{
			Transport: &rewriteTransport{
				target: target,
				base:   http.DefaultTransport,
			},
		})
		return client
	}

	driver := &Open115{Addition: Addition{
		RootID:       driverpkg.RootID{RootFolderID: "root-1"},
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		LimitRate:    limitRate,
	}}
	driver.Storage.Status = "old init error"

	if err := driver.Init(context.Background()); err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if driver.GetStorage().Status != op.WORK {
		t.Fatalf("successful authenticated Init should restore storage status to work, got %q", driver.GetStorage().Status)
	}

	mu.Lock()
	reqs := append([]recordedRequest(nil), requests...)
	mu.Unlock()

	assertRequestPaths(t, reqs, "/open/user/info", "/open/folder/get_info")
	gap := reqs[1].Time.Sub(reqs[0].Time)
	minGap := time.Duration(float64(time.Second) / limitRate * 0.7)
	if gap < minGap {
		t.Fatalf("Init SDK requests were too close (%v), expected at least %v; auth/root-info calls are not both rate-limited", gap, minGap)
	}
}

func TestOpen115RefreshRestoresErrorStateAndPublishesNewPair(t *testing.T) {
	database, err := gorm.Open(sqlite.Open("file:open115-refresh-recovery?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)

	var refreshCount int
	var rejectFresh atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/refreshToken":
			refreshCount++
			writeSDKResponse(t, w, map[string]any{
				"state": 1,
				"code":  0,
				"data": map[string]any{
					"access_token":  "fresh-access",
					"refresh_token": "fresh-refresh",
					"expires_in":    7200,
				},
			})
		case "/open/user/info":
			if r.Header.Get("Authorization") != "Bearer fresh-access" {
				writeSDKError(t, w, 40140125, "access_token invalid")
				return
			}
			if rejectFresh.Load() {
				writeSDKError(t, w, 40140120, "refresh token error")
				return
			}
			writeSDKSuccess(t, w, map[string]any{})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse server URL failed: %v", err)
	}
	oldNewClient := new115SDKClient
	t.Cleanup(func() { new115SDKClient = oldNewClient })
	new115SDKClient = func(opts ...sdk.Option) *sdk.Client {
		client := sdk.New(opts...)
		client.SetHttpClient(&http.Client{Transport: &rewriteTransport{target: target, base: http.DefaultTransport}})
		return client
	}

	driver := &Open115{Addition: Addition{
		RootID:       driverpkg.RootID{RootFolderID: "0"},
		AccessToken:  "expired-access",
		RefreshToken: "live-refresh",
	}}
	storage := model.Storage{
		Driver:    "115 Open",
		MountPath: "/refresh-recovery",
		Status:    "code: 40140125, message: access_token invalid",
		Addition:  `{"access_token":"expired-access","refresh_token":"live-refresh"}`,
	}
	if err := db.CreateStorage(&storage); err != nil {
		t.Fatalf("CreateStorage failed: %v", err)
	}
	driver.Storage = storage
	proof := make(chan op.StorageCredentialEvent, 1)
	op.RegisterStorageCredentialHook(func(typ string, event op.StorageCredentialEvent) {
		if typ != "token-valid" || event.Storage != driver {
			return
		}
		select {
		case proof <- event:
		default:
		}
	})

	if err := driver.Init(context.Background()); err != nil {
		t.Fatalf("Init after access-token expiry failed: %v", err)
	}
	if refreshCount != 1 {
		t.Fatalf("refresh count = %d, want exactly 1", refreshCount)
	}
	if driver.Addition.AccessToken != "fresh-access" || driver.Addition.RefreshToken != "fresh-refresh" {
		t.Fatalf("refreshed pair was not installed: access=%q refresh=%q", driver.Addition.AccessToken, driver.Addition.RefreshToken)
	}
	if driver.Storage.Status != op.WORK {
		t.Fatalf("refreshed mount status = %q, want WORK", driver.Storage.Status)
	}
	persisted, err := db.GetStorageById(storage.ID)
	if err != nil {
		t.Fatalf("GetStorageById failed: %v", err)
	}
	if persisted.Status != op.WORK || !strings.Contains(persisted.Addition, "fresh-access") {
		t.Fatalf("refreshed storage was not durably recovered: %#v", persisted)
	}
	select {
	case event := <-proof:
		if !strings.Contains(event.Addition, "fresh-access") || !strings.Contains(event.Addition, "fresh-refresh") {
			t.Fatalf("published stale credential generation: %s", event.Addition)
		}
	case <-time.After(time.Second):
		t.Fatal("refreshed and proven pair was not published")
	}

	// The same long-lived driver may fail hours after refreshing. The invalid
	// callback must bind to fresh-access, not the pre-refresh generation that
	// configured the client.
	rejectFresh.Store(true)
	if _, err := driver.client.UserInfo(context.Background()); err == nil {
		t.Fatal("expected the refreshed generation to become invalid")
	}
	if driver.Storage.Status == op.WORK {
		t.Fatal("refreshed generation 401 did not invalidate the mounted storage")
	}
	persisted, err = db.GetStorageById(storage.ID)
	if err != nil {
		t.Fatalf("GetStorageById after invalidation failed: %v", err)
	}
	if persisted.Status == op.WORK {
		t.Fatal("refreshed generation 401 was not persisted for peer recovery")
	}
}

// The cluster validity protocol depends on every failed request re-announcing
// invalidity while a mount is still WORK: 115's refresh_token rotates on use,
// so a peer's successful refresh can silently kill this node's pair without
// this node ever seeing a local success in between. A one-shot latch would
// report the first failure and then go quiet forever if that first
// NotifyStorageTokenInvalidWithSnapshot call happened to be dropped by its own
// generation guard (a legitimate race, see internal/op/hook.go), permanently
// stranding the mount at WORK with a dead token and never starting cluster
// recovery. This drives the real SDK client's WithOnAccessTokenInvalid closure
// through genuine failing requests (not a captured/replayed callback) to prove
// the driver keeps re-announcing invalidity for as long as the storage is
// observably still WORK, and stops once it is not.
func TestOpen115AccessTokenInvalidCallbackIsLevelTriggered(t *testing.T) {
	// A unique DB name and mount path per invocation: the shared in-memory
	// sqlite handle otherwise outlives this test within the process, so a
	// hardcoded name/path collides with a UNIQUE constraint on any repeat
	// run (e.g. `go test -count>1`), a failure mode unrelated to what this
	// test actually exercises.
	suffix := time.Now().UnixNano()
	dbName := fmt.Sprintf("file:open115-invalid-level-trigger-%d?mode=memory&cache=shared", suffix)
	mountPath := fmt.Sprintf("/invalid-level-trigger-%d", suffix)
	database, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)

	var failing atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/user/info":
			if !failing.Load() {
				writeSDKSuccess(t, w, map[string]any{})
				return
			}
			writeSDKError(t, w, 40140125, "access_token invalid")
		case "/open/refreshToken":
			// The refresh_token is permanently dead: every refresh attempt is
			// rejected terminally, exactly like the rotation scenario where a
			// peer already consumed and replaced it. This endpoint decodes into
			// AuthResp, whose State field is an int (not bool like the ordinary
			// Resp used by writeSDKError/writeSDKSuccess) — passportRequest only
			// consults Code/Message, so State is simply omitted here.
			writeSDKResponse(t, w, map[string]any{
				"code":    40140120,
				"message": "refresh token error",
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse server URL failed: %v", err)
	}
	oldNewClient := new115SDKClient
	t.Cleanup(func() { new115SDKClient = oldNewClient })
	new115SDKClient = func(opts ...sdk.Option) *sdk.Client {
		client := sdk.New(opts...)
		client.SetHttpClient(&http.Client{Transport: &rewriteTransport{target: target, base: http.DefaultTransport}})
		return client
	}

	driver := &Open115{Addition: Addition{
		RootID:       driverpkg.RootID{RootFolderID: "0"},
		AccessToken:  "fixed-access",
		RefreshToken: "fixed-refresh",
	}}
	storage := model.Storage{
		Driver:    "115 Open",
		MountPath: mountPath,
		Status:    op.WORK,
		Addition:  `{"access_token":"fixed-access","refresh_token":"fixed-refresh"}`,
	}
	if err := db.CreateStorage(&storage); err != nil {
		t.Fatalf("CreateStorage failed: %v", err)
	}
	driver.Storage = storage

	notified := make(chan struct{}, 10)
	op.RegisterStorageCredentialHook(func(typ string, event op.StorageCredentialEvent) {
		if typ == "token-invalid" && event.Storage == driver {
			notified <- struct{}{}
		}
	})

	if err := driver.Init(context.Background()); err != nil {
		t.Fatalf("Init against the healthy server failed: %v", err)
	}
	if driver.GetStorage().Status != op.WORK {
		t.Fatalf("storage status = %q before the fault injection, want %q", driver.GetStorage().Status, op.WORK)
	}
	failing.Store(true)

	waitNotified := func(step string) {
		t.Helper()
		select {
		case <-notified:
		case <-time.After(time.Second):
			t.Fatalf("%s: expected a token-invalid notification while storage was WORK", step)
		}
	}
	triggerInvalid := func(step string) {
		t.Helper()
		// SetRefreshToken with a changed value resets the SDK client's own
		// refresh-dead gate (see 115-sdk-go client.go) so each call below is a
		// fresh attempt instead of short-circuiting on the SDK's internal
		// one-shot latch — that latch is a separate, already-tracked concern
		// owned by the SDK package, not what this test exercises.
		driver.client.SetRefreshToken(step)
		if _, err := driver.client.UserInfo(context.Background()); err == nil {
			t.Fatalf("%s: expected UserInfo to fail against the dead-pair server", step)
		}
	}

	// First failure: WORK -> invalid, the ordinary transition.
	triggerInvalid("call-1")
	waitNotified("call-1")
	if driver.GetStorage().Status == op.WORK {
		t.Fatal("call-1: storage status was not marked invalid")
	}

	// Something external put the mount back at WORK (e.g. the local proof of a
	// racing generation) without this driver's own latch ever being cleared. A
	// CAS-latch bug silences every subsequent invalid callback forever here,
	// even though the mount is observably WORK again with the same dead token.
	driver.GetStorage().SetStatus(op.WORK)
	triggerInvalid("call-2")
	waitNotified("call-2")
	if driver.GetStorage().Status == op.WORK {
		t.Fatal("call-2: storage was WORK again but the repeat invalid callback did not re-notify")
	}

	// Third failure: storage is already "token invalid" (left over from call-2,
	// not reset). The driver must not re-notify — that would be a goroutine/DB
	// write storm on every failed request against an already-known-dead mount.
	triggerInvalid("call-3")
	select {
	case <-notified:
		t.Fatal("call-3: driver re-notified token-invalid while storage was already non-WORK")
	case <-time.After(200 * time.Millisecond):
	}
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

func (m *mockFileStreamer) Read(p []byte) (int, error) { return 0, io.EOF }
func (m *mockFileStreamer) Close() error               { return nil }
func (m *mockFileStreamer) Add(_ io.Closer)            {}
func (m *mockFileStreamer) AddIfCloser(_ any)          {}
func (m *mockFileStreamer) GetSize() int64             { return m.size }
func (m *mockFileStreamer) GetName() string            { return m.name }
func (m *mockFileStreamer) ModTime() time.Time         { return time.Time{} }
func (m *mockFileStreamer) CreateTime() time.Time      { return time.Time{} }
func (m *mockFileStreamer) IsDir() bool                { return false }
func (m *mockFileStreamer) GetHash() utils.HashInfo    { return m.hashInfo }
func (m *mockFileStreamer) GetID() string              { return "" }
func (m *mockFileStreamer) GetPath() string            { return "" }
func (m *mockFileStreamer) GetMimetype() string        { return "application/octet-stream" }
func (m *mockFileStreamer) NeedStore() bool            { return false }
func (m *mockFileStreamer) IsForceStreamUpload() bool  { return false }
func (m *mockFileStreamer) GetExist() model.Obj        { return nil }
func (m *mockFileStreamer) SetExist(_ model.Obj)       {}
func (m *mockFileStreamer) GetFile() model.File        { return nil }
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

func TestGetReturnsObjectNotFoundForEmptyData(t *testing.T) {
	driver, _ := newTestOpen115(t, "trash", func(w http.ResponseWriter, r *http.Request) {
		// 115 returns empty array when path doesn't exist
		writeSDKSuccess(t, w, []any{})
	})
	driver.parentPath = ""

	_, err := driver.Get(context.Background(), "/nonexistent/path")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, errs.ObjectNotFound) {
		t.Fatalf("expected ObjectNotFound, got: %v", err)
	}
}

func TestCheckUploadCallbackSuccess(t *testing.T) {
	body := []byte(`{"state":true,"code":0,"message":"success","data":{"pick_code":"abc","file_name":"test.txt","file_size":100,"file_id":"123","sha1":"da39a3ee","cid":"456"}}`)
	if err := checkUploadCallback(body); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestCheckUploadCallbackStateFalse(t *testing.T) {
	body := []byte(`{"state":false,"code":990009,"message":"upload failed"}`)
	err := checkUploadCallback(body)
	if err == nil {
		t.Fatal("expected error for state=false, got nil")
	}
	if !strings.Contains(err.Error(), "990009") || !strings.Contains(err.Error(), "upload failed") {
		t.Fatalf("error should contain code and message, got: %v", err)
	}
}

func TestCheckUploadCallbackEmptyBody(t *testing.T) {
	err := checkUploadCallback([]byte{})
	if err == nil {
		t.Fatal("expected error for empty body, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error should mention empty, got: %v", err)
	}
}

func TestCheckUploadCallbackInvalidJSON(t *testing.T) {
	err := checkUploadCallback([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("error should mention parse error, got: %v", err)
	}
}

func TestGetReturnsObjForExistingFolder(t *testing.T) {
	driver, _ := newTestOpen115(t, "trash", func(w http.ResponseWriter, r *http.Request) {
		writeSDKSuccess(t, w, map[string]any{
			"file_id":       "99999",
			"file_name":     "my_folder",
			"pick_code":     "pc-123",
			"file_category": "0", // folder; Fix 4 rejects file responses with NotImplement
		})
	})
	driver.parentPath = ""

	obj, err := driver.Get(context.Background(), "/my_folder")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if obj.GetID() != "99999" || obj.GetName() != "my_folder" {
		t.Fatalf("unexpected obj: id=%s name=%s", obj.GetID(), obj.GetName())
	}
}
