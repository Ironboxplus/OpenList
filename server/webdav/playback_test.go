package webdav

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:webdav_playback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	conf.Conf = conf.DefaultConfig("data")
	db.Init(dB)
}

// limitedFake is a storage that declares one download connection per file and
// streams a file slowly: its first bytes come at once and the rest when the test
// lets them.
type limitedFake struct {
	*fakedriver.Driver
	release chan struct{}
}

const limitedName = "FakeLimitedStream"

var limitedStorage = &limitedFake{release: make(chan struct{})}

func init() {
	op.RegisterDriver(func() driver.Driver {
		limitedStorage.Driver = fakedriver.New()
		return limitedStorage
	})
}

func (d *limitedFake) Config() driver.Config {
	return driver.Config{Name: limitedName, LocalSort: true, DefaultRoot: "/"}
}

func (d *limitedFake) MaxDownloadConnectionsPerFile() int { return 1 }

func (d *limitedFake) Link(_ context.Context, file model.Obj, _ model.LinkArgs) (*model.Link, error) {
	b, ok := d.Content(file.GetPath())
	if !ok {
		return nil, io.EOF
	}
	return &model.Link{
		ContentLength: int64(len(b)),
		RangeReader: stream.RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
			end := int64(len(b))
			if r.Length >= 0 && r.Start+r.Length < end {
				end = r.Start + r.Length
			}
			body := b[min(r.Start, end):end]
			first := min(3, len(body))
			return io.NopCloser(io.MultiReader(bytes.NewReader(body[:first]), &afterRelease{ctx: ctx, ch: d.release, rest: body[first:]})), nil
		}),
	}, nil
}

type afterRelease struct {
	ctx  context.Context
	ch   chan struct{}
	rest []byte
	done bool
}

func (a *afterRelease) Read(p []byte) (int, error) {
	if !a.done {
		select {
		case <-a.ch:
		case <-a.ctx.Done():
			return 0, a.ctx.Err()
		}
		a.done = true
	}
	n := copy(p, a.rest)
	a.rest = a.rest[n:]
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func mount(t *testing.T, driverName, mountPath string) {
	t.Helper()
	id, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: driverName, MountPath: mountPath, Addition: "{}", CacheExpiration: 30,
		Proxy: model.Proxy{WebdavPolicy: "native_proxy"},
	})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
}

func davGet(h *Handler, path string) (*httptest.ResponseRecorder, <-chan struct{}) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(context.WithValue(req.Context(), conf.UserKey, &model.User{BasePath: "/"}))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()
	return rec, done
}

func TestAWebDAVProxyStreamIsAPlaybackOfALimitedStorage(t *testing.T) {
	limitedStorage.release = make(chan struct{})
	mount(t, limitedName, "/dav-limited")
	limitedStorage.AddFile("/v.mkv", "0123456789")
	h := &Handler{LockSystem: NewMemLS()}
	const path = "/dav-limited/v.mkv"

	if stream.DownloadGate.Playing(path) {
		t.Fatal("the file is playing before anybody asked for it")
	}
	rec, done := davGet(h, path)
	deadline := time.Now().Add(5 * time.Second)
	for !stream.DownloadGate.Playing(path) {
		if time.Now().After(deadline) {
			t.Fatal("a WebDAV GET through the proxy did not register a playback")
		}
		time.Sleep(time.Millisecond)
	}
	// while it streams, a background read of the file waits for it
	src := &stream.GatedSource{Inner: stubSource{}, Key: path, Gate: stream.DownloadGate, Limit: func() int { return 1 }}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := src.ReadAt(ctx, 0, 1); err == nil {
		t.Error("a background read ran beside a WebDAV stream on a one-connection storage")
	}

	close(limitedStorage.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the WebDAV GET never finished")
	}
	if rec.Body.String() != "0123456789" {
		t.Errorf("the stream carried %q", rec.Body.String())
	}
}

func TestAWebDAVProxyStreamOfAnUnlimitedStorageRegistersNothing(t *testing.T) {
	mount(t, fakedriver.DriverName, "/dav-free")
	d, err := op.GetStorageByMountPath("/dav-free")
	if err != nil {
		t.Fatal(err)
	}
	d.(*fakedriver.Driver).AddFile("/v.mkv", "0123456789")
	h := &Handler{LockSystem: NewMemLS()}
	rec, done := davGet(h, "/dav-free/v.mkv")
	<-done
	if rec.Body.String() != "0123456789" {
		t.Fatalf("the stream carried %q (status %d)", rec.Body.String(), rec.Code)
	}
	if stream.DownloadGate.Playing("/dav-free/v.mkv") {
		t.Error("a playback of a storage with no limit was registered")
	}
}

type stubSource struct{}

func (stubSource) Size() int64 { return 1 }
func (stubSource) ReadAt(context.Context, int64, int) ([]byte, error) {
	return []byte{0}, nil
}
