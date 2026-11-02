package handles

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/bootstrap/data"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/frp"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/gin-gonic/gin"
)

const (
	fakeFRPToken  = "frp-token-fake-SECRET-0001"
	fakeFRPSecret = "frp-stcp-fake-SECRET-0002"
)

type frpEnv struct {
	t      *testing.T
	engine *gin.Engine
}

func newFRPEnv(t *testing.T) *frpEnv {
	t.Helper()
	newMediaEnv(t) // database and the real initial settings
	old := frp.Default
	frp.Default = frp.New(http.NotFoundHandler())
	t.Cleanup(func() { frp.Default.Stop(); frp.Default = old })

	e := gin.New()
	e.GET("/api/public/settings", PublicSettings)
	ad := e.Group("/api/admin")
	ad.GET("/frp", GetFRP)
	ad.POST("/frp", SaveFRP)
	ad.POST("/frp/start", StartFRP)
	ad.POST("/frp/stop", StopFRP)
	ad.GET("/frp/status", FRPStatus)
	ad.GET("/frp/runtime", FRPRuntime)
	ad.GET("/setting/get", GetSetting)
	ad.GET("/setting/list", ListSettings)
	ad.GET("/setting/default", DefaultSettings)
	ad.POST("/setting/save", SaveSettings)
	return &frpEnv{t: t, engine: e}
}

func (e *frpEnv) do(method, target string, body any) (int, string) {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// everywhere returns every response that could carry a stored secret.
func (e *frpEnv) everywhere() map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, p := range []string{
		"/api/admin/frp", "/api/admin/frp/status", "/api/admin/frp/runtime",
		"/api/admin/setting/list", "/api/admin/setting/list?group=" + "14",
		"/api/admin/setting/get?key=frp_auth_token", "/api/admin/setting/get?key=frp_proxy_secrets",
		"/api/admin/setting/get?keys=frp_auth_token,frp_proxy_secrets,frp_proxies",
		"/api/admin/setting/default", "/api/public/settings",
	} {
		_, body := e.do("GET", p, nil)
		out[p] = body
	}
	return out
}

func (e *frpEnv) save(body map[string]any) (int, string) {
	return e.do("POST", "/api/admin/frp", body)
}

func TestFRPDefaultsAreOff(t *testing.T) {
	e := newFRPEnv(t)
	_, body := e.do("GET", "/api/admin/frp", nil)
	var r struct {
		Data FRPView `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	v := r.Data
	if v.Enabled || v.TokenSet || v.State != "stopped" || !v.TLS || v.ExposeCluster || len(v.Proxies) != 0 || v.ServerPort != 7000 {
		t.Fatalf("defaults: %+v", v)
	}
	// the initial settings carry the keys, in their own group, all private
	keys := map[string]bool{}
	for _, it := range data.InitialSettings() {
		if it.Group == model.FRP {
			keys[it.Key] = true
			if it.Flag != model.PRIVATE {
				t.Errorf("%s is not PRIVATE", it.Key)
			}
		}
	}
	for _, k := range []string{conf.FRPEnabled, conf.FRPServerAddr, conf.FRPServerPort, conf.FRPTLSEnable, conf.FRPAuthToken, conf.FRPProxies, conf.FRPProxySecrets, conf.FRPExposeCluster} {
		if !keys[k] {
			t.Errorf("initial settings lack %s", k)
		}
	}
	if m := frp.Default.Status(); m.State != frp.StateStopped {
		t.Fatalf("manager: %s", m.State)
	}
}

func TestFRPSecretsNeverLeaveTheServer(t *testing.T) {
	e := newFRPEnv(t)
	code, body := e.save(map[string]any{
		"enabled": false, "server_addr": "frps.example.test", "server_port": 7000, "tls": true,
		"token": fakeFRPToken,
		"proxies": []map[string]any{
			{"name": "peer", "type": "stcp", "secret_key": fakeFRPSecret},
			{"name": "web", "type": "http", "custom_domains": []string{"a.example.test"}},
		},
	})
	if code != 200 || strings.Contains(body, fakeFRPToken) || strings.Contains(body, fakeFRPSecret) {
		t.Fatalf("save: %d %s", code, body)
	}
	// stored, and usable by the server side
	cfg, err := frp.Load()
	if err != nil || cfg.Token != fakeFRPToken || len(cfg.Proxies) != 2 || cfg.Proxies[0].SecretKey != fakeFRPSecret {
		t.Fatalf("stored: %+v %v", cfg, err)
	}
	// ...but no response shows them
	for path, resp := range e.everywhere() {
		if strings.Contains(resp, fakeFRPToken) || strings.Contains(resp, fakeFRPSecret) {
			t.Errorf("%s returns a secret", path)
		}
	}
	// and the view says they are set
	_, body = e.do("GET", "/api/admin/frp", nil)
	var r struct {
		Data FRPView `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &r)
	if !r.Data.TokenSet || len(r.Data.Proxies) != 2 || !r.Data.Proxies[0].SecretSet || r.Data.Proxies[1].SecretSet {
		t.Fatalf("view: %+v", r.Data)
	}
	// nothing frp reaches the public settings
	_, pub := e.do("GET", "/api/public/settings", nil)
	if strings.Contains(pub, "frp_") {
		t.Fatalf("public settings carry frp keys: %s", pub)
	}
}

func TestFRPSecretUpdateSemantics(t *testing.T) {
	e := newFRPEnv(t)
	base := func() map[string]any {
		return map[string]any{"enabled": false, "server_addr": "frps.example.test", "server_port": 7000, "tls": true,
			"proxies": []map[string]any{{"name": "peer", "type": "stcp"}}}
	}
	with := func(token any, key any) map[string]any {
		b := base()
		if token != nil {
			b["token"] = token
		}
		if key != nil {
			b["proxies"].([]map[string]any)[0]["secret_key"] = key
		}
		return b
	}
	check := func(wantTok, wantKey string) {
		t.Helper()
		c, err := frp.Load()
		if err != nil || c.Token != wantTok || c.Proxies[0].SecretKey != wantKey {
			t.Fatalf("stored token %q key %q, want %q / %q (%v)", c.Token, c.Proxies[0].SecretKey, wantTok, wantKey, err)
		}
	}
	e.save(with("t1", "k1"))
	check("t1", "k1")
	e.save(with(nil, nil)) // absent = keep
	check("t1", "k1")
	e.save(with("t2", "k2")) // value = replace
	check("t2", "k2")
	e.save(with("", "")) // empty = clear
	check("", "")
}

func TestFRPGenericSettingsCannotWriteSecrets(t *testing.T) {
	e := newFRPEnv(t)
	e.save(map[string]any{"enabled": false, "server_addr": "frps.example.test", "server_port": 7000, "tls": true,
		"token": fakeFRPToken, "proxies": []map[string]any{{"name": "peer", "type": "stcp", "secret_key": fakeFRPSecret}}})

	// a generic page that posts the masked values back must not wipe them, and a
	// direct write is ignored
	code, _ := e.do("POST", "/api/admin/setting/save", []map[string]any{
		{"key": conf.FRPAuthToken, "value": ""},
		{"key": conf.FRPProxySecrets, "value": "{}"},
	})
	if code != 200 {
		t.Fatalf("save: %d", code)
	}
	c, err := frp.Load()
	if err != nil || c.Token != fakeFRPToken || c.Proxies[0].SecretKey != fakeFRPSecret {
		t.Fatalf("secrets were changed through the generic API: %+v %v", c, err)
	}
	// and the stored row itself is intact
	it, err := op.GetSettingItemByKey(conf.FRPAuthToken)
	if err != nil || it.Value != fakeFRPToken {
		t.Fatalf("row: %+v %v", it, err)
	}
}

func TestFRPRejectsAnIncompleteEnabledConfig(t *testing.T) {
	e := newFRPEnv(t)
	code, body := e.save(map[string]any{"enabled": true, "server_addr": "", "server_port": 7000, "tls": true, "token": fakeFRPToken})
	if code != 200 || !strings.Contains(body, `"code":400`) || strings.Contains(body, fakeFRPToken) {
		t.Fatalf("incomplete: %d %s", code, body)
	}
	if c, _ := frp.Load(); c.Enabled || c.Token != "" {
		t.Fatalf("a rejected save stored something: %+v", c)
	}
	// a half-done config may be saved while disabled
	code, body = e.save(map[string]any{"enabled": false, "server_addr": "", "server_port": 7000, "tls": true})
	if !strings.Contains(body, `"code":200`) {
		t.Fatalf("disabled draft: %d %s", code, body)
	}
	// start with nothing usable stored is refused with a reason, secret-free
	_, body = e.do("POST", "/api/admin/frp/start", nil)
	if !strings.Contains(body, `"code":400`) || strings.Contains(body, fakeFRPToken) {
		t.Fatalf("start: %s", body)
	}
	_, body = e.do("GET", "/api/admin/frp/status", nil)
	if !strings.Contains(body, `"state":"error"`) {
		t.Fatalf("status: %s", body)
	}
	_, body = e.do("POST", "/api/admin/frp/stop", nil)
	if !strings.Contains(body, `"state":"stopped"`) {
		t.Fatalf("stop: %s", body)
	}
}

func TestFRPRuntimeLimit(t *testing.T) {
	e := newFRPEnv(t)
	for range 20 {
		frp.Default.Ring().Addf("line")
	}
	_, body := e.do("GET", "/api/admin/frp/runtime?limit=5", nil)
	var r struct {
		Data struct {
			Lines []string `json:"lines"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil || len(r.Data.Lines) != 5 {
		t.Fatalf("runtime: %v %s", err, body)
	}
}
