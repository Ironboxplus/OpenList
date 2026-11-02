package handles

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	"github.com/OpenListTeam/OpenList/v4/server/middlewares"
	"github.com/gin-gonic/gin"
)

// These two tests pin behaviour that changed with the share lifecycle work and
// use only API that exists before it, so they can be run against the older tree
// to see them fail there:
//   - HEAD is not an access;
//   - a link that fails to resolve does not consume an access.

func regressEnv(t *testing.T) (*gin.Engine, *model.User) {
	t.Helper()
	newMediaEnv(t) // database and initial settings
	admin := (&model.User{Username: "regress-admin", Role: model.ADMIN, BasePath: "/", Permission: 0x1F | 1<<14}).SetPassword("pw")
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
	st.(*fakedriver.Driver).AddFile("/a.txt", "hello world")

	e := gin.New()
	e.GET("/sd/:sid", middlewares.EmptyPathParse, middlewares.SharingIdParse, SharingDown)
	e.HEAD("/sd/:sid", middlewares.EmptyPathParse, middlewares.SharingIdParse, SharingDown)
	return e, admin
}

func regressShare(t *testing.T, admin *model.User, file string, maxAccessed int) string {
	t.Helper()
	s := &model.Sharing{SharingDB: &model.SharingDB{MaxAccessed: maxAccessed}, Files: []string{file}, Creator: admin}
	id, err := op.CreateSharing(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func regressDo(e *gin.Engine, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "8.8.8.8:1"
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func TestShareHeadIsNotAnAccess(t *testing.T) {
	e, admin := regressEnv(t)
	id := regressShare(t, admin, "/s/a.txt", 1)
	for range 3 {
		if w := regressDo(e, "HEAD", "/sd/"+id); w.Code != 200 {
			t.Fatalf("HEAD: %d", w.Code)
		}
	}
	if r, err := db.GetSharingById(id); err != nil || r.Accessed != 0 {
		t.Fatalf("HEAD took a slot: %+v %v", r, err)
	}
	// the one allowed download is still there for the real GET
	if w := regressDo(e, "GET", "/sd/"+id); w.Code != 200 {
		t.Fatalf("GET after HEAD: %d", w.Code)
	}
}

func TestShareFailedLinkDoesNotConsumeAnAccess(t *testing.T) {
	e, admin := regressEnv(t)
	id := regressShare(t, admin, "/s/missing.txt", 1)
	if w := regressDo(e, "GET", "/sd/"+id); w.Code == 200 {
		t.Fatalf("a missing file was served")
	}
	if r, err := db.GetSharingById(id); err != nil || r.Accessed != 0 {
		t.Fatalf("a failed link consumed the access: %+v %v", r, err)
	}
}
