package cstcloud_capsule

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

const multistatus = `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/dav/</d:href>
    <d:propstat>
      <d:prop><d:resourcetype><d:collection/></d:resourcetype><d:displayname></d:displayname></d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/hello.txt</d:href>
    <d:propstat>
      <d:prop>
        <d:resourcetype/>
        <d:displayname>hello.txt</d:displayname>
        <d:getcontentlength>5</d:getcontentlength>
        <d:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</d:getlastmodified>
      </d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
  </d:response>
</d:multistatus>`

// stub mimicking the DC WebDAV endpoint: Basic auth guarded PROPFIND with
// the same client-type gate as the real server (UA must contain the app
// type the credential was created for, case-insensitive)
func newStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "spaceuser" || pass != "spacepass" {
			w.Header().Set("WWW-Authenticate", `Basic realm="DC WebDAV"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(strings.ToLower(r.UserAgent()), "zotero") {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "Client type mismatch.")
			return
		}
		if r.Method != "PROPFIND" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(207)
		fmt.Fprint(w, multistatus)
	}))
}

func setup(t *testing.T, username, password string) *CSTCloudCapsule {
	t.Helper()
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig(t.TempDir())
	}
	srv := newStub(t)
	t.Cleanup(srv.Close)
	old := webdavAddress
	webdavAddress = srv.URL + "/dav"
	t.Cleanup(func() { webdavAddress = old })

	d := &CSTCloudCapsule{}
	d.Username = username
	d.Password = password
	d.UserAgent = "Mozilla/5.0 (compatible; Zotero/8.0) AList"
	d.RootFolderPath = "/"
	return d
}

func TestInitRejectsMismatchedClientType(t *testing.T) {
	d := setup(t, "spaceuser", "spacepass")
	d.UserAgent = "gowebdav"
	if err := d.Init(context.Background()); err == nil {
		t.Fatal("Init succeeded with non-matching User-Agent, want client type error")
	}
}

func TestInitRejectsBadCredentials(t *testing.T) {
	d := setup(t, "spaceuser", "wrong")
	if err := d.Init(context.Background()); err == nil {
		t.Fatal("Init succeeded with wrong password, want error")
	}
}

func TestInitAndList(t *testing.T) {
	d := setup(t, "spaceuser", "spacepass")
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	objs, err := d.List(context.Background(), &model.Object{Path: "/"}, model.ListArgs{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("List returned %d objects, want 1", len(objs))
	}
	if objs[0].GetName() != "hello.txt" || objs[0].GetSize() != 5 || objs[0].IsDir() {
		t.Fatalf("unexpected object: name=%s size=%d dir=%v", objs[0].GetName(), objs[0].GetSize(), objs[0].IsDir())
	}
}
