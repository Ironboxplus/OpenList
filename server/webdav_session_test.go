package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/session"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// WebDAV logs in with Basic auth or the static token; neither is a device, so
// neither may create a session or be refused by max_devices.
func TestWebDAVAuthCreatesNoSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conf.Conf = conf.DefaultConfig(t.TempDir())
	d, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:webdav-session-%d?mode=memory&cache=shared", time.Now().UnixNano())),
		&gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "x_"}})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(d)
	session.Reset()
	t.Cleanup(session.Reset)

	u := (&model.User{Username: "dav-alice", Role: model.GENERAL, BasePath: "/", Permission: 0x3FFFF}).SetPassword("pw")
	if err := db.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	_ = op.DelUserCache(u.Username)
	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.MaxDevices, Value: "1", Type: conf.TypeNumber, Group: model.GLOBAL}); err != nil {
		t.Fatal(err)
	}
	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.Token, Value: "static-token", Type: conf.TypeString, Group: model.SINGLE}); err != nil {
		t.Fatal(err)
	}
	admin := (&model.User{Username: "dav-boss", Role: model.ADMIN, BasePath: "/", Permission: 0x3FFFF}).SetPassword("pw")
	if err := db.CreateUser(admin); err != nil {
		t.Fatal(err)
	}

	if _, err := session.Create(u.ID, session.Meta{}); err != nil { // the one allowed device is taken
		t.Fatal(err)
	}
	count := func() int64 {
		var n int64
		if err := db.GetDb().Model(&model.Session{}).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()

	e := gin.New()
	e.Any("/dav/*path", WebDAVAuth, func(c *gin.Context) { c.Status(http.StatusMultiStatus) })
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "/dav/", nil)
		req.SetBasicAuth("dav-alice", "pw")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		if w.Code != http.StatusMultiStatus {
			t.Fatalf("basic request %d: HTTP %d, want 207 (max_devices must not apply)", i, w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/dav/", nil)
	req.Header.Set("Authorization", "Bearer static-token")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("static token: HTTP %d", w.Code)
	}
	if got := count(); got != before {
		t.Fatalf("WebDAV requests changed the sessions table: %d -> %d", before, got)
	}
}
