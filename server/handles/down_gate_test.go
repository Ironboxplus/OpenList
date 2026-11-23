package handles

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	"github.com/OpenListTeam/OpenList/v4/server/middlewares"
	"github.com/gin-gonic/gin"
)

// A storage that allows two connections per file (as 115 does) and answers 403
// to a third, whoever opens them: a copy task, and a player on /d at the same
// time. The generic read layer keeps every connection to the file within the
// number the storage declares, so nobody is refused.

const gateFileSize = 64 << 20

type gateRig struct {
	t        *testing.T
	src, dst *fakedriver.Driver
	data     []byte
	server   *httptest.Server
}

// newGateRig mounts the limited storage at /gsrc holding /big.bin, and an empty
// one at /gdst. concurrency is what the source's links ask of the downloader.
func newGateRig(t *testing.T, concurrency int) *gateRig {
	t.Helper()
	newMediaEnv(t) // database and settings
	ctx := context.Background()
	mount := func(path string, webProxy bool) *fakedriver.Driver {
		id, err := op.CreateStorage(ctx, model.Storage{Driver: fakedriver.DriverName, MountPath: path, Addition: "{}", CacheExpiration: 30, WebProxy: webProxy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = op.DeleteStorageById(ctx, id) })
		st, err := op.GetStorageByMountPath(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.(*fakedriver.Driver)
	}
	r := &gateRig{t: t, src: mount("/gsrc", true), dst: mount("/gdst", false)}
	r.data = make([]byte, gateFileSize)
	_, _ = rand.Read(r.data)
	r.src.AddFile("/big.bin", string(r.data))
	r.src.PerFile = 2
	r.src.LinkConcurrency, r.src.LinkPartSize = concurrency, 16<<20
	r.src.ReadDelay = time.Millisecond // 64 KiB: a part of 16 MiB takes a quarter of a second

	e := gin.New()
	e.GET("/d/*path", middlewares.PathParse, Down)
	r.server = httptest.NewServer(e)
	t.Cleanup(r.server.Close)
	return r
}

// copy is a copy task of /gsrc/big.bin into /gdst, run as the task manager does.
func (r *gateRig) copy() error {
	tk := &fs.FileTransferTask{TaskData: fs.TaskData{
		SrcStorage: r.src, DstStorage: r.dst,
		SrcActualPath: "/big.bin", DstActualPath: "/",
		SrcStorageMp: "/gsrc", DstStorageMp: "/gdst",
	}}
	tk.SetCtx(context.Background())
	return tk.RunWithNextTaskCallback(func(*fs.FileTransferTask) error { return nil })
}

func (r *gateRig) checkCopied() {
	r.t.Helper()
	got, ok := r.dst.Content("/big.bin")
	if !ok {
		r.t.Fatal("the copy is not in the destination")
	}
	if sha256.Sum256(got) != sha256.Sum256(r.data) {
		r.t.Error("the copy differs from the source")
	}
}

func (r *gateRig) checkConnections(want int) {
	r.t.Helper()
	peak := r.src.PeakOpen("/big.bin")
	r.t.Logf("most connections open to the file at once: %d, refused: %d", peak, r.src.Refused())
	if n := r.src.Refused(); n != 0 {
		r.t.Errorf("the storage refused %d connections beyond its 2 per file", n)
	}
	if want > 0 && peak != want {
		r.t.Errorf("%d connections were open at once, want %d", peak, want)
	}
	if want == 0 && peak > 2 {
		r.t.Errorf("%d connections were open at once, the storage allows 2", peak)
	}
}

func TestCopyTaskUsesTheConnectionsTheStorageAllows(t *testing.T) {
	for name, concurrency := range map[string]int{
		// the link asks for what the storage allows
		"as declared": 2,
		// the link asks for more: the layer keeps to what the storage declares
		"more than declared": 4,
	} {
		t.Run(name, func(t *testing.T) {
			r := newGateRig(t, concurrency)
			if err := r.copy(); err != nil {
				t.Fatalf("copy: %v", err)
			}
			r.checkCopied()
			r.checkConnections(2)
		})
	}
}

// A player starts on the file while it is being copied: together they never
// hold more than the 2 connections, the player is not refused, and the copy
// still ends with the right content.
func TestPlaybackDuringCopyKeepsWithinTheStorageLimit(t *testing.T) {
	r := newGateRig(t, 2)

	var wg sync.WaitGroup
	var copyErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		copyErr = r.copy()
	}()
	for deadline := time.Now().Add(30 * time.Second); r.src.PeakOpen("/big.bin") == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the copy did not start reading")
		}
	}

	const played = 24 << 20
	req, _ := http.NewRequest("GET", r.server.URL+"/d/gsrc/big.bin?d", nil)
	req.Header.Set("Range", "bytes=0-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("playback got status %d", resp.StatusCode)
	}
	got, err := io.ReadAll(io.LimitReader(resp.Body, played))
	resp.Body.Close()
	if err != nil || len(got) != played {
		t.Fatalf("playback read %d bytes, err %v", len(got), err)
	}
	if sha256.Sum256(got) != sha256.Sum256(r.data[:played]) {
		t.Error("the played bytes differ from the source")
	}

	wg.Wait()
	if copyErr != nil {
		t.Fatalf("copy: %v", copyErr)
	}
	r.checkCopied()
	r.checkConnections(0)
}
