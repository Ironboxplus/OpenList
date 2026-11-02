package handles

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// virtual_path is the path the requesting user addresses the object by in the
// other APIs, whatever the driver puts in its own path (AList e36c68e4). For a
// user with a base path it is relative to that base, not the server path.
func TestFsResponsesCarryVirtualPath(t *testing.T) {
	e := newShareEnv(t) // storage mounted at /s holding /s/a.txt
	e.fake.AddDir("/dir")
	e.fake.AddFile("/dir/in.txt", "x")

	call := func(user *model.User, path, body string) map[string]any {
		t.Helper()
		g := gin.New()
		g.Use(func(c *gin.Context) { common.GinAppendValues(c, conf.UserKey, user); c.Next() })
		g.POST("/api/fs/get", FsGetSplit)
		g.POST("/api/fs/list", FsListSplit)
		req := httptest.NewRequest("POST", path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		var r struct {
			Code int            `json:"code"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.Code != 200 {
			t.Fatalf("%s %s: %v %s", path, body, err, w.Body.String())
		}
		return r.Data
	}
	vp := func(m map[string]any) string { s, _ := m["virtual_path"].(string); return s }

	// admin at the root: fs/get and every entry of fs/list
	if got := vp(call(e.admin, "/api/fs/get", `{"path":"/s/a.txt"}`)); got != "/s/a.txt" {
		t.Errorf("fs/get: %q", got)
	}
	list := call(e.admin, "/api/fs/list", `{"path":"/s"}`)
	seen := map[string]bool{}
	for _, it := range list["content"].([]any) {
		seen[vp(it.(map[string]any))] = true
	}
	if !seen["/s/a.txt"] || !seen["/s/dir"] {
		t.Errorf("fs/list virtual paths: %v", seen)
	}
	// a trailing slash and a dirty path come out clean
	if got := vp(call(e.admin, "/api/fs/get", `{"path":"/s//dir/"}`)); got != "/s/dir" {
		t.Errorf("dirty path: %q", got)
	}

	// a user whose base path is /s sees /a.txt, not /s/a.txt
	based := *e.admin
	based.Role, based.BasePath = model.GENERAL, "/s"
	if got := vp(call(&based, "/api/fs/get", `{"path":"/a.txt"}`)); got != "/a.txt" {
		t.Errorf("base-path user fs/get: %q", got)
	}
	list = call(&based, "/api/fs/list", `{"path":"/dir"}`)
	items := list["content"].([]any)
	if len(items) != 1 || vp(items[0].(map[string]any)) != "/dir/in.txt" {
		t.Errorf("base-path user fs/list: %v", items)
	}
}
