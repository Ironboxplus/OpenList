package chunker

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func init() {
	d, err := gorm.Open(sqlite.Open("file:chunker-test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig("data")
	db.Init(d)
}

func mount(t *testing.T, driverName, mountPath, addition string) {
	t.Helper()
	id, err := op.CreateStorage(context.Background(), model.Storage{Driver: driverName, MountPath: mountPath, Addition: addition, CacheExpiration: 30})
	if err != nil {
		t.Fatalf("mount %s: %v", mountPath, err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
}

// Chunks of one file are written one at a time and in order, also when they are
// spread over several targets (the targets may be 115 accounts, whose uploads
// must never run concurrently: PLAN_GLUE_ALIST §5).
func TestChunksAreUploadedStrictlySequentially(t *testing.T) {
	mount(t, fakedriver.DriverName, "/t1", "{}")
	mount(t, fakedriver.DriverName, "/t2", "{}")
	var t1, t2 *fakedriver.Driver
	for path, dst := range map[string]**fakedriver.Driver{"/t1": &t1, "/t2": &t2} {
		s, err := op.GetStorageByMountPath(path)
		if err != nil {
			t.Fatal(err)
		}
		*dst = s.(*fakedriver.Driver)
	}

	var mu sync.Mutex
	inflight, maxInflight := 0, 0
	var order []string
	hook := func(p string) error {
		mu.Lock()
		inflight++
		maxInflight = max(maxInflight, inflight)
		order = append(order, p)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond) // an overlapping Put would show up here
		mu.Lock()
		inflight--
		mu.Unlock()
		return nil
	}
	t1.FailPut, t2.FailPut = hook, hook
	t1.AddDir("/c")
	t2.AddDir("/c")

	mount(t, "Chunker", "/chunked",
		`{"remote_path":"/t1/c","remote_paths":"/t2/c","store_chunks_in_primary":true,"chunk_size":4,"name_format":"{name}.rclone_chunk.{chunk:3}","start_from":1,"meta_format":"simplejson","hash_type":"md5"}`)
	st, err := op.GetStorageByMountPath("/chunked")
	if err != nil {
		t.Fatal(err)
	}
	content := "0123456789abcdefghij" // 20 bytes = 5 chunks of 4
	file := &stream.FileStream{
		Obj:    &model.Object{Name: "big.bin", Size: int64(len(content)), HashInfo: utils.NewHashInfo(nil, "")},
		Reader: strings.NewReader(content),
	}
	if err := op.Put(context.Background(), st, "/", file, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if maxInflight != 1 {
		t.Fatalf("%d uploads ran at the same time, want strictly 1", maxInflight)
	}
	var chunks []string
	for _, p := range order {
		if strings.Contains(p, "rclone_chunk") {
			chunks = append(chunks, filepath.Base(p))
		}
	}
	want := []string{"big.bin.rclone_chunk.001", "big.bin.rclone_chunk.002", "big.bin.rclone_chunk.003", "big.bin.rclone_chunk.004", "big.bin.rclone_chunk.005"}
	if len(chunks) != len(want) {
		t.Fatalf("chunks written: %v, want %v", chunks, want)
	}
	for i := range want {
		if !strings.HasPrefix(chunks[i], want[i]) { // a transaction suffix follows the number
			t.Fatalf("chunk order: %v, want %v", chunks, want)
		}
	}
	// the chunks really went to both targets, alternating
	if t1.Puts.Load() < 3 || t2.Puts.Load() < 2 {
		t.Fatalf("puts: t1=%d t2=%d", t1.Puts.Load(), t2.Puts.Load())
	}
}

// The property above holds by construction: the driver starts no goroutines and
// uses no errgroup/WaitGroup. Anyone adding concurrency has to come here and
// read why it must not (115: sequential multipart, no concurrent uploads).
func TestDriverNeverUploadsConcurrently(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatal("no sources", err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			if strings.Contains(imp.Path.Value, "errgroup") {
				t.Errorf("%s imports %s: chunks must be uploaded one at a time", f, imp.Path.Value)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if g, ok := n.(*ast.GoStmt); ok {
				t.Errorf("%s: go statement at %s: chunks must be uploaded one at a time", f, fset.Position(g.Pos()))
			}
			return true
		})
	}
}
