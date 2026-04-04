package fs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// ---------- helpers ----------

func dirObj(name string) model.Obj {
	return &model.Object{Name: name, IsFolder: true}
}

func fileObj(name string) model.Obj {
	return &model.Object{Name: name, IsFolder: false}
}

// callRecorder records every path passed to makeDir and listSrc.
type callRecorder struct {
	mu     sync.Mutex
	mkdirs []string
	lists  []string
	// listReturns maps srcPath → objects to return (nil = empty)
	listReturns map[string][]model.Obj
	// mkdirErr maps dstPath → error to return
	mkdirErr map[string]error
}

func newRecorder() *callRecorder {
	return &callRecorder{
		listReturns: make(map[string][]model.Obj),
		mkdirErr:    make(map[string]error),
	}
}

func (r *callRecorder) makeDir(_ context.Context, path string) error {
	r.mu.Lock()
	r.mkdirs = append(r.mkdirs, path)
	err := r.mkdirErr[path]
	r.mu.Unlock()
	return err
}

func (r *callRecorder) listSrc(_ context.Context, path string) ([]model.Obj, error) {
	r.mu.Lock()
	r.lists = append(r.lists, path)
	objs := r.listReturns[path]
	r.mu.Unlock()
	return objs, nil
}

func (r *callRecorder) hasMkdir(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.mkdirs {
		if p == path {
			return true
		}
	}
	return false
}

func (r *callRecorder) hasList(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.lists {
		if p == path {
			return true
		}
	}
	return false
}

// ---------- tests ----------

// TestPreCreateDirTreeFn_EmptyObjs: no objects → no calls at all.
func TestPreCreateDirTreeFn_EmptyObjs(t *testing.T) {
	rec := newRecorder()
	err := preCreateDirTreeFn(context.Background(), nil, "/src", "/dst", 1, rec.makeDir, rec.listSrc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.mkdirs) != 0 {
		t.Errorf("expected 0 MakeDir calls, got %d: %v", len(rec.mkdirs), rec.mkdirs)
	}
	if len(rec.lists) != 0 {
		t.Errorf("expected 0 List calls, got %d: %v", len(rec.lists), rec.lists)
	}
}

// TestPreCreateDirTreeFn_OnlyFiles: file objects only → zero MakeDir calls.
func TestPreCreateDirTreeFn_OnlyFiles(t *testing.T) {
	objs := []model.Obj{fileObj("a.txt"), fileObj("b.txt")}
	rec := newRecorder()
	if err := preCreateDirTreeFn(context.Background(), objs, "/src", "/dst", 1, rec.makeDir, rec.listSrc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.mkdirs) != 0 {
		t.Errorf("expected 0 MakeDir calls, got %d", len(rec.mkdirs))
	}
}

// TestPreCreateDirTreeFn_FlatDirs_MaxDepth0: dirs present, maxDepth=0 → MakeDir
// called for each dir with correct dstPath, NO listSrc calls.
func TestPreCreateDirTreeFn_FlatDirs_MaxDepth0(t *testing.T) {
	objs := []model.Obj{dirObj("subA"), fileObj("file.txt"), dirObj("subB")}
	rec := newRecorder()
	if err := preCreateDirTreeFn(context.Background(), objs, "/src", "/dst/parent", 0, rec.makeDir, rec.listSrc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !rec.hasMkdir("/dst/parent/subA") {
		t.Error("expected MakeDir(/dst/parent/subA)")
	}
	if !rec.hasMkdir("/dst/parent/subB") {
		t.Error("expected MakeDir(/dst/parent/subB)")
	}
	if rec.hasMkdir("/dst/parent/file.txt") {
		t.Error("MakeDir must NOT be called for a file")
	}
	if len(rec.lists) != 0 {
		t.Errorf("maxDepth=0 must not trigger any List calls, got: %v", rec.lists)
	}
}

// TestPreCreateDirTreeFn_Recursion_CorrectSrcPath is the regression test for the
// srcBasePath bug: with maxDepth=1 the recursive List must use the SUBDIR src path,
// not the original top-level srcBasePath.
func TestPreCreateDirTreeFn_Recursion_CorrectSrcPath(t *testing.T) {
	// /src/parent contains [subA(dir), subB(dir)]
	// /src/parent/subA contains [subA1(dir)]
	// /src/parent/subB contains []
	topObjs := []model.Obj{dirObj("subA"), dirObj("subB")}
	rec := newRecorder()
	rec.listReturns["/src/parent/subA"] = []model.Obj{dirObj("subA1")}
	rec.listReturns["/src/parent/subB"] = []model.Obj{}

	if err := preCreateDirTreeFn(context.Background(), topObjs, "/src/parent", "/dst/parent", 1, rec.makeDir, rec.listSrc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// ── first level dirs must be created
	if !rec.hasMkdir("/dst/parent/subA") {
		t.Error("expected MakeDir(/dst/parent/subA)")
	}
	if !rec.hasMkdir("/dst/parent/subB") {
		t.Error("expected MakeDir(/dst/parent/subB)")
	}

	// ── listSrc must use subdirSrcPath (NOT the whole /src/parent again)
	if !rec.hasList("/src/parent/subA") {
		t.Error("listSrc must be called with /src/parent/subA, got:", rec.lists)
	}
	if !rec.hasList("/src/parent/subB") {
		t.Error("listSrc must be called with /src/parent/subB, got:", rec.lists)
	}
	// The original bug would have called listSrc("/src/parent/subA") as
	// stdpath.Join(t.SrcActualPath, "subA") where t.SrcActualPath=="/src/parent",
	// but in a deeper recursive call (e.g. maxDepth=2) it would have used
	// the top-level path incorrectly; verify the nested mkdir used the right dst.
	if !rec.hasMkdir("/dst/parent/subA/subA1") {
		t.Error("expected MakeDir(/dst/parent/subA/subA1), got mkdirs:", rec.mkdirs)
	}
}

// TestPreCreateDirTreeFn_MaxDepth1_NoFurtherRecursion: with maxDepth=1 recursion
// goes exactly one level. The nested list returns another dir, but since maxDepth
// reaches 0 that deeper dir must NOT be listed further.
func TestPreCreateDirTreeFn_MaxDepth1_NoFurtherRecursion(t *testing.T) {
	topObjs := []model.Obj{dirObj("sub")}
	rec := newRecorder()
	// sub contains deeper, deeper contains deepest
	rec.listReturns["/src/sub"] = []model.Obj{dirObj("deeper")}
	rec.listReturns["/src/sub/deeper"] = []model.Obj{dirObj("deepest")} // should NOT be listed

	if err := preCreateDirTreeFn(context.Background(), topObjs, "/src", "/dst", 1, rec.makeDir, rec.listSrc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !rec.hasMkdir("/dst/sub") {
		t.Error("expected /dst/sub to be created")
	}
	if !rec.hasMkdir("/dst/sub/deeper") {
		t.Error("expected /dst/sub/deeper to be created (within maxDepth=1)")
	}
	// deepest must NOT be created (would require maxDepth=2)
	if rec.hasMkdir("/dst/sub/deeper/deepest") {
		t.Error("/dst/sub/deeper/deepest must NOT be created at maxDepth=1")
	}
	// /src/sub/deeper must NOT be listed (we've hit maxDepth=0 at that point)
	if rec.hasList("/src/sub/deeper") {
		t.Error("/src/sub/deeper must NOT be listed when maxDepth reaches 0")
	}
}

// TestPreCreateDirTreeFn_ContextCancelled: context cancelled before processing →
// returns ctx.Err, makes zero or partial calls.
func TestPreCreateDirTreeFn_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	objs := []model.Obj{dirObj("sub")}
	rec := newRecorder()
	err := preCreateDirTreeFn(ctx, objs, "/src", "/dst", 1, rec.makeDir, rec.listSrc)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
	if len(rec.mkdirs) != 0 {
		t.Errorf("no MakeDir should be called after cancellation, got: %v", rec.mkdirs)
	}
}

// TestPreCreateDirTreeFn_ContextCancelledDuringRecursion: context is cancelled
// during the second-pass recursion loop.
func TestPreCreateDirTreeFn_ContextCancelledDuringRecursion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Two dirs; cancel after the first List in recursion
	callCount := 0
	listSrc := func(c context.Context, path string) ([]model.Obj, error) {
		callCount++
		cancel() // cancel on first list call
		return nil, nil
	}
	objs := []model.Obj{dirObj("sub1"), dirObj("sub2")}
	rec := newRecorder()
	err := preCreateDirTreeFn(ctx, objs, "/src", "/dst", 1, rec.makeDir, listSrc)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled after cancellation during recursion, got: %v", err)
	}
	if callCount > 1 {
		t.Errorf("listSrc should have been called at most once before ctx.Err fired, got %d", callCount)
	}
}

// TestPreCreateDirTreeFn_MakeDirErrorNonFatal: a MakeDir failure on one dir must
// not stop processing of subsequent dirs.
func TestPreCreateDirTreeFn_MakeDirErrorNonFatal(t *testing.T) {
	objs := []model.Obj{dirObj("subA"), dirObj("subB"), dirObj("subC")}
	rec := newRecorder()
	rec.mkdirErr["/dst/subA"] = errors.New("quota exceeded")

	if err := preCreateDirTreeFn(context.Background(), objs, "/src", "/dst", 0, rec.makeDir, rec.listSrc); err != nil {
		t.Fatalf("error should not propagate from MakeDir failure: %v", err)
	}
	// All three must have been attempted despite the error on subA
	for _, p := range []string{"/dst/subA", "/dst/subB", "/dst/subC"} {
		if !rec.hasMkdir(p) {
			t.Errorf("expected MakeDir(%s) to be called", p)
		}
	}
}

// TestPreCreateDirTreeFn_ListErrorNonFatal: a List error for one subdir during
// recursion skips that subdir but continues with the rest.
func TestPreCreateDirTreeFn_ListErrorNonFatal(t *testing.T) {
	objs := []model.Obj{dirObj("subA"), dirObj("subB")}
	listCallCount := 0
	listSrc := func(_ context.Context, path string) ([]model.Obj, error) {
		listCallCount++
		if path == "/src/subA" {
			return nil, errors.New("I/O error")
		}
		return []model.Obj{dirObj("nested")}, nil
	}
	rec := newRecorder()
	if err := preCreateDirTreeFn(context.Background(), objs, "/src", "/dst", 1, rec.makeDir, listSrc); err != nil {
		t.Fatalf("List error must not be fatal: %v", err)
	}
	// subB's nested dir should still be processed despite subA's List failure
	if !rec.hasMkdir("/dst/subB/nested") {
		t.Error("expected /dst/subB/nested to be created despite subA list error, mkdirs:", rec.mkdirs)
	}
	if listCallCount != 2 {
		t.Errorf("both subdirs must be attempted for listing, got %d calls", listCallCount)
	}
}

// TestPreCreateDirTreeFn_MixedObjs: mixed files and dirs; only dirs are processed.
func TestPreCreateDirTreeFn_MixedObjs(t *testing.T) {
	objs := []model.Obj{
		fileObj("readme.md"),
		dirObj("assets"),
		fileObj("main.go"),
		dirObj("pkg"),
	}
	rec := newRecorder()
	if err := preCreateDirTreeFn(context.Background(), objs, "/src", "/dst", 0, rec.makeDir, rec.listSrc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.mkdirs) != 2 {
		t.Errorf("expected exactly 2 MakeDir calls, got %d: %v", len(rec.mkdirs), rec.mkdirs)
	}
	if !rec.hasMkdir("/dst/assets") || !rec.hasMkdir("/dst/pkg") {
		t.Errorf("unexpected mkdirs: %v", rec.mkdirs)
	}
}

// TestPreCreateDirTreeFn_Timeout: context with a very short deadline cancels execution.
func TestPreCreateDirTreeFn_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	time.Sleep(5 * time.Millisecond) // ensure deadline has passed

	objs := []model.Obj{dirObj("sub")}
	rec := newRecorder()
	err := preCreateDirTreeFn(ctx, objs, "/src", "/dst", 1, rec.makeDir, rec.listSrc)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got: %v", err)
	}
}
