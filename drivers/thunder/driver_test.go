package thunder

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	// 初始化内存数据库，保证 op.MustSaveDriverStorage 在测试中安全执行
	conf.Conf = conf.DefaultConfig(os.TempDir())
	d, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	db.Init(d)
	os.Exit(m.Run())
}

// mockServer 记录收到的请求路径，并按路径返回模拟响应
type mockServer struct {
	mu      sync.Mutex
	paths   []string
	handler func(path string, w http.ResponseWriter, r *http.Request)
	server  *httptest.Server
	orig    string
}

func newMockServer() *mockServer {
	ms := &mockServer{}
	ms.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ms.mu.Lock()
		ms.paths = append(ms.paths, r.URL.Path)
		ms.mu.Unlock()
		ms.handler(r.URL.Path, w, r)
	}))
	// 两个 URL 变量都是包加载时初始化的，必须一并覆盖
	ms.orig = XLUSER_API_BASE_URL
	XLUSER_API_BASE_URL = ms.server.URL
	XLUSER_API_URL = ms.server.URL + "/v1"
	return ms
}

func (ms *mockServer) Close() {
	XLUSER_API_BASE_URL = ms.orig
	XLUSER_API_URL = ms.orig + "/v1"
	ms.server.Close()
}

func (ms *mockServer) Paths() []string {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return append([]string(nil), ms.paths...)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestThunder(mountPath string) *Thunder {
	return &Thunder{
		Storage: model.Storage{MountPath: mountPath},
		Addition: Addition{
			Username:     "13800138000",
			Password:     "secret",
			DeviceID:     "0123456789abcdef0123456789abcdef",
			CaptchaToken: "",
			CreditKey:    "",
		},
	}
}

func TestThunderInitRestoresSessionWithRefreshToken(t *testing.T) {
	ms := newMockServer()
	ms.handler = func(path string, w http.ResponseWriter, r *http.Request) {
		switch path {
		case "/v1/auth/token":
			writeJSON(w, TokenResp{
				TokenType: "Bearer", AccessToken: "at-1", RefreshToken: "rt-new",
				ExpiresIn: 7200, UserID: "u1",
			})
		default:
			http.NotFound(w, r)
		}
	}
	defer ms.Close()

	x := newTestThunder("thunder-test-refresh")
	x.Addition.RefreshToken = "rt-old"
	x.Addition.CreditKey = "consumed-credit-key"

	if err := x.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 只应调用 refresh token 接口，不应走账号密码登录
	if got := ms.Paths(); !reflect.DeepEqual(got, []string{"/v1/auth/token"}) {
		t.Fatalf("unexpected requests: %v", got)
	}
	if x.Addition.RefreshToken != "rt-new" {
		t.Fatalf("RefreshToken = %q, want %q", x.Addition.RefreshToken, "rt-new")
	}
	if x.Addition.CreditKey != "" {
		t.Fatalf("CreditKey = %q, want cleared", x.Addition.CreditKey)
	}
	if x.Token() != "Bearer at-1" {
		t.Fatalf("Token() = %q, want %q", x.Token(), "Bearer at-1")
	}
}

func TestThunderInitRestoresSessionWithoutRotatedRefreshToken(t *testing.T) {
	ms := newMockServer()
	ms.handler = func(path string, w http.ResponseWriter, r *http.Request) {
		switch path {
		case "/v1/auth/token":
			// 服务端刷新成功但未轮换 refresh token（响应里该字段为空）
			writeJSON(w, TokenResp{
				TokenType: "Bearer", AccessToken: "at-1",
				ExpiresIn: 7200, UserID: "u1",
			})
		default:
			http.NotFound(w, r)
		}
	}
	defer ms.Close()

	x := newTestThunder("thunder-test-no-rotate")
	x.Addition.RefreshToken = "rt-old"

	if err := x.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 刷新成功：不应回退到账号密码登录
	if got := ms.Paths(); !reflect.DeepEqual(got, []string{"/v1/auth/token"}) {
		t.Fatalf("unexpected requests: %v", got)
	}
	// refresh token 未轮换时应保留旧值
	if x.Addition.RefreshToken != "rt-old" {
		t.Fatalf("RefreshToken = %q, want preserved %q", x.Addition.RefreshToken, "rt-old")
	}
	if x.Token() != "Bearer at-1" {
		t.Fatalf("Token() = %q, want %q", x.Token(), "Bearer at-1")
	}
}

func TestThunderInitFallsBackToLoginAndPersists(t *testing.T) {
	ms := newMockServer()
	ms.handler = func(path string, w http.ResponseWriter, r *http.Request) {
		switch path {
		case "/v1/auth/token":
			// refresh token 已失效
			writeJSON(w, ErrResp{ErrorCode: 4001, ErrorMsg: "invalid_grant"})
		case "/xluser.core.login/v3/login":
			writeJSON(w, CoreLoginResp{SessionID: "sid-1", UserID: "u1"})
		case "/v1/shield/captcha/init":
			writeJSON(w, CaptchaTokenResponse{CaptchaToken: "ct-1", ExpiresIn: 3600})
		case "/v1/auth/signin/token":
			writeJSON(w, TokenResp{
				TokenType: "Bearer", AccessToken: "at-2", RefreshToken: "rt-2",
				ExpiresIn: 7200, UserID: "u1",
			})
		default:
			http.NotFound(w, r)
		}
	}
	defer ms.Close()

	x := newTestThunder("thunder-test-login")
	x.Addition.RefreshToken = "rt-stale"
	x.Addition.CreditKey = "consumed-credit-key"

	if err := x.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	want := []string{
		"/v1/auth/token",
		"/xluser.core.login/v3/login",
		"/v1/shield/captcha/init",
		"/v1/auth/signin/token",
	}
	if got := ms.Paths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected requests: %v", got)
	}
	if x.Addition.CreditKey != "" {
		t.Fatalf("CreditKey = %q, want cleared", x.Addition.CreditKey)
	}
	if x.Addition.RefreshToken != "rt-2" {
		t.Fatalf("RefreshToken = %q, want %q", x.Addition.RefreshToken, "rt-2")
	}
	if x.Addition.CaptchaToken != "ct-1" {
		t.Fatalf("CaptchaToken = %q, want %q", x.Addition.CaptchaToken, "ct-1")
	}
	if x.Token() != "Bearer at-2" {
		t.Fatalf("Token() = %q, want %q", x.Token(), "Bearer at-2")
	}
}

func TestThunderRefreshTokenFuncPersistsRotatedToken(t *testing.T) {
	var (
		meCalls  int
		tokCalls int
	)
	ms := newMockServer()
	ms.handler = func(path string, w http.ResponseWriter, r *http.Request) {
		switch path {
		case "/v1/auth/token":
			ms.mu.Lock()
			tokCalls++
			call := tokCalls
			ms.mu.Unlock()
			if call == 1 {
				// Init 阶段：通过 refresh token 恢复登录
				writeJSON(w, TokenResp{
					TokenType: "Bearer", AccessToken: "at-1", RefreshToken: "rt-new",
					ExpiresIn: 7200, UserID: "u1",
				})
				return
			}
			// refreshTokenFunc 阶段：服务端轮换 refresh token
			writeJSON(w, TokenResp{
				TokenType: "Bearer", AccessToken: "at-rotated", RefreshToken: "rt-rotated",
				ExpiresIn: 7200, UserID: "u1",
			})
		case "/v1/user/me":
			ms.mu.Lock()
			meCalls++
			call := meCalls
			ms.mu.Unlock()
			if call == 1 {
				// 第一次返回 access token 过期，触发 refreshTokenFunc
				writeJSON(w, ErrResp{ErrorCode: 4122, ErrorMsg: "access token expired"})
				return
			}
			writeJSON(w, map[string]string{"userID": "u1"})
		default:
			http.NotFound(w, r)
		}
	}
	defer ms.Close()

	x := newTestThunder("thunder-test-rotate")
	x.Addition.RefreshToken = "rt-old"

	if err := x.Init(nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if x.Addition.RefreshToken != "rt-new" {
		t.Fatalf("RefreshToken after Init = %q, want %q", x.Addition.RefreshToken, "rt-new")
	}
	if !x.IsLogin() {
		t.Fatal("IsLogin() = false, want true")
	}
	// IsLogin 触发 4122 → refreshTokenFunc → 轮换后的 refresh token 应持久化
	if x.Addition.RefreshToken != "rt-rotated" {
		t.Fatalf("RefreshToken = %q, want rotated %q", x.Addition.RefreshToken, "rt-rotated")
	}
	if x.Token() != "Bearer at-rotated" {
		t.Fatalf("Token() = %q, want %q", x.Token(), "Bearer at-rotated")
	}
}

func TestRequestReturnsErrorWhenTokenIsMissing(t *testing.T) {
	xc := &XunLeiCommon{}

	_, err := xc.Request(API_URL+"/about", http.MethodGet, nil, nil)
	if !errors.Is(err, errs.EmptyToken) {
		t.Fatalf("expected EmptyToken, got %v", err)
	}
}
