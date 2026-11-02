package handles

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/session"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/OpenListTeam/OpenList/v4/server/middlewares"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
)

// sessEnv is the real login handlers behind the real Auth middleware, on a
// database with the production table prefix and the real initial settings.
type sessEnv struct {
	t      *testing.T
	engine *gin.Engine
}

func newSessEnv(t *testing.T) *sessEnv {
	t.Helper()
	newMediaEnv(t) // database, initial settings (max_devices and friends included)
	session.Reset()
	common.SecretKey = []byte("session-test-secret")
	t.Cleanup(session.Reset)

	for _, u := range []*model.User{
		(&model.User{Username: "alice", Role: model.GENERAL, BasePath: "/"}).SetPassword("pw"),
		(&model.User{Username: "bob", Role: model.GENERAL, BasePath: "/"}).SetPassword("pw"),
		(&model.User{Username: "boss", Role: model.ADMIN, BasePath: "/", Permission: 0x1F}).SetPassword("pw"),
		(&model.User{Username: "guest", Role: model.GUEST, BasePath: "/"}).SetPassword(""),
	} {
		if err := db.CreateUser(u); err != nil {
			t.Fatal(err)
		}
		_ = op.DelUserCache(u.Username) // earlier tests used other databases
	}

	e := gin.New()
	api := e.Group("/api")
	api.POST("/auth/login/hash", LoginHash)
	auth := api.Group("", middlewares.Auth(false))
	auth.GET("/me", CurrentUser)
	auth.GET("/auth/logout", LogOut)
	auth.GET("/me/sessions", ListMySessions)
	auth.POST("/me/sessions/evict", EvictMySession)
	adm := auth.Group("/admin", middlewares.AuthAdmin)
	adm.GET("/session/list", ListSessions)
	adm.POST("/session/evict", EvictSession)
	return &sessEnv{t: t, engine: e}
}

func (m *sessEnv) setting(key, value string) {
	m.t.Helper()
	it, err := op.GetSettingItemByKey(key)
	if err != nil {
		m.t.Fatalf("setting %s: %v", key, err)
	}
	c := *it
	c.Value = value
	if err := op.SaveSettingItem(&c); err != nil {
		m.t.Fatal(err)
	}
}

type sessResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (m *sessEnv) do(method, path, token string, body any, hdr ...string) (int, sessResp) {
	m.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sess-test")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	m.engine.ServeHTTP(w, req)
	var r sessResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		m.t.Fatalf("%s %s: HTTP %d, body %q", method, path, w.Code, w.Body.String())
	}
	return w.Code, r
}

// login returns the token and session id; code is the response code.
func (m *sessEnv) login(user string, hdr ...string) (token, sessionID string, code int) {
	m.t.Helper()
	_, r := m.do("POST", "/api/auth/login/hash", "", map[string]string{"username": user, "password": model.StaticHash("pw")}, hdr...)
	var d struct {
		Token     string `json:"token"`
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(r.Data, &d)
	return d.Token, d.SessionID, r.Code
}

func (m *sessEnv) mustLogin(user string, hdr ...string) (string, string) {
	m.t.Helper()
	tok, id, code := m.login(user, hdr...)
	if code != 200 || tok == "" || id == "" {
		m.t.Fatalf("login %s: code %d", user, code)
	}
	return tok, id
}

func (m *sessEnv) me(token string) int {
	m.t.Helper()
	_, r := m.do("GET", "/api/me", token, nil)
	return r.Code
}

func sessionRows(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := db.GetDb().Model(&model.Session{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLoginCarriesASessionIDInTheToken(t *testing.T) {
	m := newSessEnv(t)
	tok, id := m.mustLogin("alice")
	claims, err := common.ParseToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.ID != id {
		t.Fatalf("jti %q, session_id %q", claims.ID, id)
	}
	if _, err := db.GetSession(id); err != nil {
		t.Fatalf("no row for the session: %v", err)
	}
	if m.me(tok) != 200 {
		t.Fatal("fresh token rejected")
	}
}

func TestTokenWithoutAJTIIsRejected(t *testing.T) {
	m := newSessEnv(t)
	// a token as issued before sessions existed: valid signature, no jti
	old, err := jwt.NewWithClaims(jwt.SigningMethodHS256, common.UserClaims{
		Username: "alice", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString(common.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	if code := m.me(old); code != 401 {
		t.Fatalf("old token: code %d, want 401", code)
	}
}

func TestLogoutEndsOnlyThatSession(t *testing.T) {
	m := newSessEnv(t)
	a, _ := m.mustLogin("alice")
	b, _ := m.mustLogin("alice")
	if _, r := m.do("GET", "/api/auth/logout", a, nil); r.Code != 200 {
		t.Fatalf("logout: %d", r.Code)
	}
	if got := m.me(a); got != 401 {
		t.Fatalf("logged-out token: code %d, want 401", got)
	}
	if got := m.me(b); got != 200 {
		t.Fatalf("the other device: code %d, want 200", got)
	}
}

func TestDenyPolicyRefusesTheLoginWith403(t *testing.T) {
	m := newSessEnv(t)
	m.setting(conf.MaxDevices, "1")
	first, _ := m.mustLogin("alice")
	if _, _, code := m.login("alice"); code != 403 {
		t.Fatalf("second login: code %d, want 403", code)
	}
	if m.me(first) != 200 {
		t.Fatal("the first device was disturbed by the refused login")
	}
	// the limit applies to administrators too
	m.mustLogin("boss")
	if _, _, code := m.login("boss"); code != 403 {
		t.Fatalf("second admin login: code %d, want 403", code)
	}
	// and not to somebody else's count
	m.mustLogin("bob")
}

func TestEvictOldestAnswers401ToTheEvictedDevice(t *testing.T) {
	m := newSessEnv(t)
	m.setting(conf.MaxDevices, "1")
	m.setting(conf.DeviceEvictPolicy, "evict_oldest")
	first, _ := m.mustLogin("alice")
	second, _ := m.mustLogin("alice")
	if got := m.me(first); got != 401 {
		t.Fatalf("evicted device: code %d, want 401 (not 403)", got)
	}
	if got := m.me(second); got != 200 {
		t.Fatalf("new device: code %d, want 200", got)
	}
}

func TestClientIDReusesTheDeviceSlot(t *testing.T) {
	m := newSessEnv(t)
	m.setting(conf.MaxDevices, "1")
	first, _ := m.mustLogin("alice", "Client-Id", "phone")
	second, _ := m.mustLogin("alice", "Client-Id", "phone")
	if m.me(first) != 401 || m.me(second) != 200 {
		t.Fatal("the returning device must replace its old session")
	}
	if _, _, code := m.login("alice", "Client-Id", "tablet"); code != 403 {
		t.Fatalf("another device: code %d, want 403", code)
	}
}

func TestUsersListAndEvictTheirOwnDevicesOnly(t *testing.T) {
	m := newSessEnv(t)
	a1, id1 := m.mustLogin("alice")
	a2, id2 := m.mustLogin("alice")
	b1, idb := m.mustLogin("bob")

	_, r := m.do("GET", "/api/me/sessions", a1, nil)
	var list []SessionResp
	_ = json.Unmarshal(r.Data, &list)
	if len(list) != 2 {
		t.Fatalf("alice sees %d devices, want 2: %s", len(list), r.Data)
	}
	for _, s := range list {
		if s.Current != (s.SessionID == id1) {
			t.Fatalf("current flag wrong for %s", s.SessionID)
		}
		if s.UA != "sess-test" {
			t.Fatalf("ua = %q", s.UA)
		}
	}

	// alice cannot touch bob's device
	if _, r := m.do("POST", "/api/me/sessions/evict", a1, map[string]string{"session_id": idb}); r.Code == 200 {
		t.Fatal("evicted another user's device")
	}
	if m.me(b1) != 200 {
		t.Fatal("bob's device died")
	}
	// but her own other device, yes
	if _, r := m.do("POST", "/api/me/sessions/evict", a1, map[string]string{"session_id": id2}); r.Code != 200 {
		t.Fatalf("evict own device: %d %s", r.Code, r.Message)
	}
	if m.me(a2) != 401 || m.me(a1) != 200 {
		t.Fatal("only the evicted device should be signed out")
	}
}

func TestAdminListsAndEvictsAnyDevice(t *testing.T) {
	m := newSessEnv(t)
	boss, _ := m.mustLogin("boss")
	a, ida := m.mustLogin("alice")

	if _, r := m.do("GET", "/api/admin/session/list", a, nil); r.Code != 403 {
		t.Fatalf("non-admin list: %d, want 403", r.Code)
	}
	_, r := m.do("GET", "/api/admin/session/list", boss, nil)
	var list []SessionResp
	_ = json.Unmarshal(r.Data, &list)
	if len(list) != 2 || list[0].UserID == 0 {
		t.Fatalf("admin list: %s", r.Data)
	}
	if _, r := m.do("POST", "/api/admin/session/evict", boss, map[string]string{"session_id": ida}); r.Code != 200 {
		t.Fatalf("admin evict: %d %s", r.Code, r.Message)
	}
	if m.me(a) != 401 {
		t.Fatal("evicted by admin but still working")
	}
}

func TestGuestAndAdminTokenCreateNoSessionAndIgnoreTheLimit(t *testing.T) {
	m := newSessEnv(t)
	m.setting(conf.MaxDevices, "1")
	m.mustLogin("alice")
	rows := sessionRows(t)

	for i := 0; i < 5; i++ {
		if got := m.me(""); got != 200 { // no token: guest
			t.Fatalf("guest /me: %d", got)
		}
	}
	static := m.staticToken()
	for i := 0; i < 5; i++ {
		if got := m.me(static); got != 200 {
			t.Fatalf("static admin token /me: %d", got)
		}
	}
	if got := sessionRows(t); got != rows {
		t.Fatalf("guest / admin-token requests changed the sessions table: %d -> %d", rows, got)
	}
}

func (m *sessEnv) staticToken() string {
	it, err := op.GetSettingItemByKey(conf.Token)
	if err != nil {
		m.t.Fatal(err)
	}
	return it.Value
}

func TestRequestsDoNotWriteTheSessionRow(t *testing.T) {
	m := newSessEnv(t)
	tok, id := m.mustLogin("alice")
	before, _ := db.GetSession(id)
	for i := 0; i < 100; i++ {
		if m.me(tok) != 200 {
			t.Fatalf("request %d rejected", i)
		}
	}
	after, _ := db.GetSession(id)
	if after.LastActive != before.LastActive || after.UserAgent != before.UserAgent {
		t.Fatalf("100 requests inside the interval wrote the row: %+v -> %+v", before, after)
	}
}
