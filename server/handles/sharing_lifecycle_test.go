package handles

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/OpenListTeam/OpenList/v4/server/middlewares"
	"github.com/gin-gonic/gin"
)

type shareEnv struct {
	t      *testing.T
	engine *gin.Engine
	fake   *fakedriver.Driver
	admin  *model.User
}

// newShareEnv is the real share download, auth, create and update handlers on a
// database with the production prefix and settings, over an in-memory storage
// mounted at /s holding /s/a.txt = "hello world".
func newShareEnv(t *testing.T) *shareEnv {
	t.Helper()
	newMediaEnv(t) // database and initial settings
	admin := (&model.User{Username: "share-admin", Role: model.ADMIN, BasePath: "/", Permission: 0x1F | 1<<14}).SetPassword("pw")
	if err := db.CreateUser(admin); err != nil {
		t.Fatal(err)
	}
	_ = op.DelUserCache(admin.Username)
	admin, err := db.GetUserByName(admin.Username)
	if err != nil {
		t.Fatal(err)
	}

	id, err := op.CreateStorage(context.Background(), model.Storage{Driver: fakedriver.DriverName, MountPath: "/s", Addition: "{}", CacheExpiration: 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	st, err := op.GetStorageByMountPath("/s")
	if err != nil {
		t.Fatal(err)
	}
	fake := st.(*fakedriver.Driver)
	fake.AddFile("/a.txt", "hello world")

	e := gin.New()
	e.Use(func(c *gin.Context) {
		common.GinAppendValues(c, conf.UserKey, admin)
		c.Next()
	})
	e.GET("/sd/:sid", middlewares.EmptyPathParse, middlewares.SharingIdParse, SharingDown)
	e.GET("/sd/:sid/*path", middlewares.PathParse, middlewares.SharingIdParse, SharingDown)
	e.HEAD("/sd/:sid", middlewares.EmptyPathParse, middlewares.SharingIdParse, SharingDown)
	e.HEAD("/sd/:sid/*path", middlewares.PathParse, middlewares.SharingIdParse, SharingDown)
	e.POST("/api/public/sharing_auth", SharingAuth)
	e.POST("/api/share/create", CreateSharing)
	e.POST("/api/share/update", UpdateSharing)
	return &shareEnv{t: t, engine: e, fake: fake, admin: admin}
}

func (e *shareEnv) do(method, target string, body any, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, rd)
	req.RemoteAddr = "9.9.9.9:1234"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

// share makes a share of /s/a.txt (or file) directly in the database.
func (e *shareEnv) share(file string, mod func(*model.SharingDB)) *model.Sharing {
	e.t.Helper()
	s := &model.Sharing{SharingDB: &model.SharingDB{}, Files: []string{file}, Creator: e.admin}
	if mod != nil {
		mod(s.SharingDB)
	}
	id, err := op.CreateSharing(s)
	if err != nil {
		e.t.Fatal(err)
	}
	s.ID = id
	return s
}

func (e *shareEnv) row(id string) *model.SharingDB {
	e.t.Helper()
	r, err := db.GetSharingById(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestShareDownloadChargesOncePerVisitor(t *testing.T) {
	e := newShareEnv(t)
	s := e.share("/s/a.txt", func(d *model.SharingDB) { d.SetPassword("pw"); d.MaxAccessed = 1 })
	url := "/sd/" + s.ID

	if w := e.do("GET", url, nil); w.Code != 403 {
		t.Fatalf("no password: %d", w.Code)
	}
	if w := e.do("GET", url+"?pwd=bad", nil); w.Code != 403 {
		t.Fatalf("wrong password: %d", w.Code)
	}
	if r := e.row(s.ID); r.Accessed != 0 {
		t.Fatalf("refused requests took a slot: %d", r.Accessed)
	}

	w := e.do("GET", url+"?pwd=pw", nil)
	if w.Code != 200 || w.Body.String() != "hello world" {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}
	if r := e.row(s.ID); r.Accessed != 1 || r.DownloadCount != 1 || r.ConsumedAt == nil {
		t.Fatalf("after download: accessed %d downloads %d consumed %v", r.Accessed, r.DownloadCount, r.ConsumedAt)
	}

	// HEAD never counts; a ranged continuation is the same visit, not a new download
	if w := e.do("HEAD", url+"?pwd=pw", nil); w.Code != 200 {
		t.Fatalf("HEAD: %d", w.Code)
	}
	w = e.do("GET", url+"?pwd=pw", nil, "Range", "bytes=6-")
	if w.Code != 206 || w.Body.String() != "world" {
		t.Fatalf("range: %d %q", w.Code, w.Body.String())
	}
	if r := e.row(s.ID); r.Accessed != 1 || r.DownloadCount != 1 {
		t.Fatalf("HEAD/range counted: accessed %d downloads %d", r.Accessed, r.DownloadCount)
	}
}

func TestShareFailedLinkCostsNoSlot(t *testing.T) {
	e := newShareEnv(t)
	s := e.share("/s/missing.txt", func(d *model.SharingDB) { d.BurnAfterRead = true })
	if w := e.do("GET", "/sd/"+s.ID, nil); w.Code == 200 {
		t.Fatalf("a missing file was served: %d", w.Code)
	}
	if r := e.row(s.ID); r.Accessed != 0 || r.DownloadCount != 0 || r.ConsumedAt != nil {
		t.Fatalf("failed link burned the share: %+v", r)
	}
	e.fake.AddFile("/missing.txt", "now here")
	if w := e.do("GET", "/sd/"+s.ID, nil); w.Code != 200 {
		t.Fatalf("after the file appeared: %d", w.Code)
	}
	if r := e.row(s.ID); r.Accessed != 1 {
		t.Fatalf("accessed = %d", r.Accessed)
	}
}

func TestSharePreviewDownloadSwitchesOverHTTP(t *testing.T) {
	e := newShareEnv(t)
	s := e.share("/s/a.txt", func(d *model.SharingDB) { d.DenyDownload = true })
	url := "/sd/" + s.ID
	if w := e.do("GET", url, nil); w.Code != 403 {
		t.Fatalf("download of a preview-only share: %d", w.Code)
	}
	if w := e.do("GET", url+"?preview=1", nil); w.Code != 200 {
		t.Fatalf("preview of a preview-only share: %d", w.Code)
	}
	if w := e.do("GET", url+"?type=thumb", nil); w.Code != 200 {
		t.Fatalf("thumbnail of a preview-only share: %d", w.Code)
	}
	if r := e.row(s.ID); r.DownloadCount != 0 {
		t.Fatalf("previews counted as downloads: %d", r.DownloadCount)
	}

	p := e.share("/s/a.txt", func(d *model.SharingDB) { d.DenyPreview = true })
	if w := e.do("GET", "/sd/"+p.ID+"?preview=1", nil); w.Code != 403 {
		t.Fatalf("preview of a download-only share: %d", w.Code)
	}
	if w := e.do("GET", "/sd/"+p.ID, nil); w.Code != 200 {
		t.Fatalf("download of a download-only share: %d", w.Code)
	}
}

func TestShareAuthTokenInsteadOfPasswordInLinks(t *testing.T) {
	e := newShareEnv(t)
	s := e.share("/s/a.txt", func(d *model.SharingDB) { d.SetPassword("p&w #1") })

	if w := e.do("POST", "/api/public/sharing_auth", map[string]string{"id": s.ID, "password": "nope"}); w.Code != 403 && !strings.Contains(w.Body.String(), "403") {
		t.Fatalf("wrong password: %d %s", w.Code, w.Body.String())
	}
	w := e.do("POST", "/api/public/sharing_auth", map[string]string{"id": s.ID, "password": "p&w #1"})
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Code != 200 || resp.Data.Token == "" {
		t.Fatalf("auth: %d %s", w.Code, w.Body.String())
	}

	link := shareLink("http://h", "sd", s.SharingDB, s.ID, "")
	if strings.Contains(link, "p&w") || strings.Contains(link, "p%26w") || !strings.Contains(link, "?auth=") {
		t.Fatalf("link carries the password or no token: %s", link)
	}
	// the generated link downloads; so does the header form
	if w := e.do("GET", strings.TrimPrefix(link, "http://h"), nil); w.Code != 200 {
		t.Fatalf("link: %d", w.Code)
	}
	if w := e.do("GET", "/sd/"+s.ID, nil, "X-Share-Token", resp.Data.Token); w.Code != 200 {
		t.Fatalf("header token: %d", w.Code)
	}
	if w := e.do("GET", "/sd/"+s.ID+"?auth="+resp.Data.Token+"x", nil); w.Code != 403 {
		t.Fatalf("forged token: %d", w.Code)
	}
}

func TestShareWrongPasswordLockoutOverHTTP(t *testing.T) {
	e := newShareEnv(t)
	s := e.share("/s/a.txt", func(d *model.SharingDB) { d.SetPassword("pw") })
	for i := range model.DefaultMaxAuthRetries {
		if w := e.do("GET", "/sd/"+s.ID+"?pwd=bad", nil); w.Code != 403 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w := e.do("GET", "/sd/"+s.ID+"?pwd=pw", nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("locked: %d", w.Code)
	}
}

func TestShareCreateAndUpdateFields(t *testing.T) {
	e := newShareEnv(t)
	type resp struct {
		Code int `json:"code"`
		Data struct {
			ID            string `json:"id"`
			HasPassword   bool   `json:"has_password"`
			AllowPreview  bool   `json:"allow_preview"`
			AllowDownload bool   `json:"allow_download"`
			BurnAfterRead bool   `json:"burn_after_read"`
		} `json:"data"`
	}
	call := func(path string, body map[string]any) resp {
		t.Helper()
		w := e.do("POST", path, body)
		var r resp
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return r
	}

	c := call("/api/share/create", map[string]any{"files": []string{"/s/a.txt"}, "pwd": "pw", "burn_after_read": true, "allow_download": false})
	id := c.Data.ID
	if !c.Data.HasPassword || !c.Data.BurnAfterRead || c.Data.AllowDownload || !c.Data.AllowPreview {
		t.Fatalf("create response: %+v", c.Data)
	}
	row := e.row(id)
	if row.PasswordHash == "" || row.Pwd != "pw" || !row.DenyDownload || row.DenyPreview {
		t.Fatalf("stored: %+v", row)
	}
	salt := row.PasswordSalt

	// an update that repeats the password and omits the new fields changes none of them
	u := call("/api/share/update", map[string]any{"id": id, "files": []string{"/s/a.txt"}, "pwd": "pw", "remark": "r"})
	if !u.Data.BurnAfterRead || u.Data.AllowDownload {
		t.Fatalf("update dropped the flags: %+v", u.Data)
	}
	if r := e.row(id); r.PasswordSalt != salt || r.Remark != "r" {
		t.Fatalf("same password re-salted or edit lost: %+v", r)
	}

	// a different password re-salts (old tokens die); an empty one opens the share
	call("/api/share/update", map[string]any{"id": id, "files": []string{"/s/a.txt"}, "pwd": "pw2"})
	if r := e.row(id); r.PasswordSalt == salt || !r.VerifyPassword("pw2") || r.VerifyPassword("pw") {
		t.Fatalf("password change: %+v", r)
	}
	o := call("/api/share/update", map[string]any{"id": id, "files": []string{"/s/a.txt"}, "pwd": ""})
	if o.Data.HasPassword {
		t.Fatalf("cleared password still reported: %+v", o.Data)
	}

	// a consumed share opens again when its limit is raised
	if _, err := db.TakeSharingSlot(id); err != nil {
		t.Fatal(err)
	}
	if e.row(id).ConsumedAt == nil {
		t.Fatal("burn share not consumed")
	}
	call("/api/share/update", map[string]any{"id": id, "files": []string{"/s/a.txt"}, "burn_after_read": false})
	if r := e.row(id); r.ConsumedAt != nil || r.LimitReached() {
		t.Fatalf("lifting the limit left the share consumed: %+v", r)
	}
}
