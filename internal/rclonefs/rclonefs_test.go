package rclonefs_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/rclonefs"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/glebarez/sqlite"
	_ "github.com/rclone/rclone/backend/local"
	rfs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/sync"
	"gorm.io/gorm"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:rclonefs_test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig(os.TempDir())
	db.Init(dB)
}

// mountLocal mounts a Local storage over a fresh temp dir at mountPath and
// returns the directory, so tests can look at what actually landed on disk.
func mountLocal(t *testing.T, mountPath string) string {
	t.Helper()
	dir := t.TempDir()
	addition := `{"root_folder_path":` + quote(dir) + `}`
	id, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mountPath, Addition: addition,
	})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	return dir
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, root, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

func localFs(t *testing.T, dir string) rfs.Fs {
	t.Helper()
	f, err := rfs.NewFs(context.Background(), dir)
	if err != nil {
		t.Fatalf("local fs: %v", err)
	}
	return f
}

func TestSyncLocalToOpenListMirrorsAddsChangesAndDeletes(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	dst := mountLocal(t, "/sync-dst")
	writeFile(t, src, "a.txt", "alpha")
	writeFile(t, src, "sub/b.txt", "bravo")
	writeFile(t, src, "sub/deeper/c.txt", "charlie")

	olFs, err := rclonefs.NewFs(ctx, "/sync-dst")
	if err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	if err := sync.Sync(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	for rel, want := range map[string]string{"a.txt": "alpha", "sub/b.txt": "bravo", "sub/deeper/c.txt": "charlie"} {
		if got, ok := readFile(t, dst, rel); !ok || got != want {
			t.Fatalf("%s after first sync = %q (exists=%v), want %q", rel, got, ok, want)
		}
	}

	writeFile(t, src, "a.txt", "alpha, edited")
	if err := os.Remove(filepath.Join(src, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, src, "new.txt", "new")
	if err := sync.Sync(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got, _ := readFile(t, dst, "a.txt"); got != "alpha, edited" {
		t.Fatalf("a.txt = %q, want the edited content", got)
	}
	if _, ok := readFile(t, dst, "sub/b.txt"); ok {
		t.Fatalf("sub/b.txt still exists after it was deleted at the source")
	}
	if got, _ := readFile(t, dst, "new.txt"); got != "new" {
		t.Fatalf("new.txt = %q", got)
	}
}

func TestCopyOpenListToLocalReadsThroughLinks(t *testing.T) {
	ctx := context.Background()
	srcDir := mountLocal(t, "/sync-src")
	writeFile(t, srcDir, "x/y.bin", "payload bytes")
	dst := t.TempDir()

	olFs, err := rclonefs.NewFs(ctx, "/sync-src")
	if err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	if err := sync.CopyDir(ctx, localFs(t, dst), olFs, false); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if got, _ := readFile(t, dst, "x/y.bin"); got != "payload bytes" {
		t.Fatalf("x/y.bin = %q", got)
	}
}

func TestObjectOpenHonoursRangeOption(t *testing.T) {
	ctx := context.Background()
	srcDir := mountLocal(t, "/range-src")
	writeFile(t, srcDir, "r.txt", "0123456789")

	olFs, err := rclonefs.NewFs(ctx, "/range-src")
	if err != nil {
		t.Fatal(err)
	}
	obj, err := olFs.NewObject(ctx, "r.txt")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	rc, err := obj.Open(ctx, &rfs.RangeOption{Start: 3, End: 6})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "3456" {
		t.Fatalf("range 3-6 = %q, want 3456", b)
	}
}

func TestNewObjectAndListReportMissingPaths(t *testing.T) {
	ctx := context.Background()
	mountLocal(t, "/missing")
	olFs, err := rclonefs.NewFs(ctx, "/missing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := olFs.NewObject(ctx, "nope.txt"); err != rfs.ErrorObjectNotFound {
		t.Fatalf("NewObject(missing) err = %v, want ErrorObjectNotFound", err)
	}
	if _, err := olFs.List(ctx, "no-such-dir"); err != rfs.ErrorDirNotFound {
		t.Fatalf("List(missing) err = %v, want ErrorDirNotFound", err)
	}
}

// The 115 driver only skips caching the whole file when it is handed a
// *stream.SeekableStream it can range-read, and only skips hashing when SHA1
// is already in the object's HashInfo. A source rclone can range-read must be
// handed over exactly that way — and without consuming the sequential `in`,
// which rclone opened before calling Put.
func TestPutStreamFromRangeableSourceIsSeekableWithSHA1AndLeavesInUnread(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, src, "f.bin", "hello rapid upload")
	lf := localFs(t, src)
	srcObj, err := lf.NewObject(ctx, "f.bin")
	if err != nil {
		t.Fatal(err)
	}
	wantSHA1, err := srcObj.Hash(ctx, hash.SHA1)
	if err != nil {
		t.Fatal(err)
	}

	in := &countingReader{r: strings.NewReader("hello rapid upload")}
	fileStream, err := rclonefs.NewPutStream(ctx, in, srcObj, "f.bin")
	if err != nil {
		t.Fatalf("NewPutStream: %v", err)
	}
	ss, ok := fileStream.(*stream.SeekableStream)
	if !ok {
		t.Fatalf("stream type = %T, want *stream.SeekableStream", fileStream)
	}
	if got := ss.GetHash().GetHash(utils.SHA1); got != wantSHA1 {
		t.Fatalf("prefilled SHA1 = %q, want %q", got, wantSHA1)
	}
	r, err := ss.RangeRead(http_range.Range{Start: 6, Length: 5})
	if err != nil {
		t.Fatalf("RangeRead: %v", err)
	}
	b, _ := io.ReadAll(r)
	if string(b) != "rapid" {
		t.Fatalf("RangeRead(6,5) = %q, want rapid", b)
	}
	if in.n != 0 {
		t.Fatalf("Put consumed %d bytes of the sequential input; it must range-read the source instead", in.n)
	}
	_ = ss.Close()
}

func TestCheckPassesAfterSync(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	mountLocal(t, "/check-dst")
	writeFile(t, src, "k.txt", "kilo")
	olFs, err := rclonefs.NewFs(ctx, "/check-dst")
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.Sync(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatal(err)
	}
	if err := operations.Check(ctx, &operations.CheckOpt{Fdst: olFs, Fsrc: localFs(t, src), OneWay: true}); err != nil {
		t.Fatalf("check after sync: %v", err)
	}
}

func TestUploadsAreReportedWithBytesReadFromSource(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	mountLocal(t, "/observed")
	writeFile(t, src, "big.bin", strings.Repeat("x", 300*1024))
	var got []rclonefs.Upload
	ctx = rclonefs.WithUploadObserver(ctx, func(u rclonefs.Upload) { got = append(got, u) })
	olFs, err := rclonefs.NewFs(ctx, "/observed")
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Remote != "big.bin" || got[0].Err != nil {
		t.Fatalf("uploads reported = %+v", got)
	}
	// The Local driver has no rapid upload: it must read the whole file, and
	// that must not be mistaken for a hash hit.
	if got[0].SourceRead < got[0].Size || got[0].Rapid() {
		t.Fatalf("full copy reported as read=%d size=%d rapid=%v", got[0].SourceRead, got[0].Size, got[0].Rapid())
	}
	if !(rclonefs.Upload{Size: 300 * 1024, SourceRead: 128 * 1024}).Rapid() {
		t.Fatalf("an upload that only read the hash prefix is not reported as rapid")
	}
}

func TestMoveAndDirMoveWithinOneStorage(t *testing.T) {
	ctx := context.Background()
	dir := mountLocal(t, "/mv")
	writeFile(t, dir, "a/one.txt", "1")
	writeFile(t, dir, "d/two.txt", "2")
	olFs, err := rclonefs.NewFs(ctx, "/mv")
	if err != nil {
		t.Fatal(err)
	}
	if olFs.Features().Move == nil || olFs.Features().DirMove == nil {
		t.Fatalf("Move/DirMove not advertised; --track-renames would re-upload")
	}
	src, err := olFs.NewObject(ctx, "a/one.txt")
	if err != nil {
		t.Fatal(err)
	}
	moved, err := olFs.Features().Move(ctx, src, "b/renamed.txt")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if moved.Remote() != "b/renamed.txt" {
		t.Fatalf("moved remote = %q", moved.Remote())
	}
	if got, _ := readFile(t, dir, "b/renamed.txt"); got != "1" {
		t.Fatalf("b/renamed.txt = %q", got)
	}
	if _, ok := readFile(t, dir, "a/one.txt"); ok {
		t.Fatalf("source still present after Move")
	}
	if err := olFs.Features().DirMove(ctx, olFs, "d", "e"); err != nil {
		t.Fatalf("DirMove: %v", err)
	}
	if got, _ := readFile(t, dir, "e/two.txt"); got != "2" {
		t.Fatalf("e/two.txt = %q", got)
	}
	if err := olFs.Features().DirMove(ctx, olFs, "e", "b"); err != rfs.ErrorDirExists {
		t.Fatalf("DirMove onto an existing dir err = %v, want ErrorDirExists", err)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
