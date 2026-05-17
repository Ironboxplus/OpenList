package fs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	pkgerrors "github.com/pkg/errors"
)

// ---------- helpers ----------

func dirObj(name string) model.Obj {
	return &model.Object{Name: name, IsFolder: true}
}

func fileObj(name string) model.Obj {
	return &model.Object{Name: name, IsFolder: false}
}

// listRecorder records every call to listDst so tests can assert on
// invocation patterns when needed.
type listRecorder struct {
	mu       sync.Mutex
	calls    []string
	respond  func(path string) ([]model.Obj, error)
}

func newListRecorder(respond func(string) ([]model.Obj, error)) *listRecorder {
	return &listRecorder{respond: respond}
}

func (r *listRecorder) listDst(_ context.Context, path string) ([]model.Obj, error) {
	r.mu.Lock()
	r.calls = append(r.calls, path)
	r.mu.Unlock()
	return r.respond(path)
}

// ---------- tests: existingDstFilesFn ----------
//
// These tests pin down the contract of the merge-mode existedObjs builder
// extracted from RunWithNextTaskCallback. They are the regression guard
// for the deletion of the BFS precreate logic: the only invariant that
// the BFS precreate quietly protected (and that #1898's ObjectNotFound
// tolerance independently fixes) lives in this function.

// 1. dst exists but is empty → empty map, no error.
func TestExistingDstFilesFn_EmptyDst(t *testing.T) {
	rec := newListRecorder(func(string) ([]model.Obj, error) { return nil, nil })
	got, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

// 2. dst contains only files → all included.
func TestExistingDstFilesFn_OnlyFiles(t *testing.T) {
	rec := newListRecorder(func(string) ([]model.Obj, error) {
		return []model.Obj{fileObj("a.txt"), fileObj("b.txt"), fileObj("c.txt")}, nil
	})
	got, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if !got[name] {
			t.Errorf("expected %q in existed set, got %v", name, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("expected 3 entries, got %d: %v", len(got), got)
	}
}

// 3. dst contains only directories → empty map (dirs don't count as
//    "existed" because merge only skips already-uploaded *files*).
func TestExistingDstFilesFn_OnlyDirs(t *testing.T) {
	rec := newListRecorder(func(string) ([]model.Obj, error) {
		return []model.Obj{dirObj("subA"), dirObj("subB")}, nil
	})
	got, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("dirs must not appear in existed-files map, got %v", got)
	}
}

// 4. dst contains mixed files and dirs → only files are recorded.
func TestExistingDstFilesFn_MixedFilesAndDirs(t *testing.T) {
	rec := newListRecorder(func(string) ([]model.Obj, error) {
		return []model.Obj{
			fileObj("readme.md"),
			dirObj("assets"),
			fileObj("main.go"),
			dirObj("pkg"),
			fileObj("go.mod"),
		}, nil
	})
	got, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 file entries, got %d: %v", len(got), got)
	}
	for _, name := range []string{"readme.md", "main.go", "go.mod"} {
		if !got[name] {
			t.Errorf("expected %q in existed set, got %v", name, got)
		}
	}
	for _, name := range []string{"assets", "pkg"} {
		if got[name] {
			t.Errorf("dir %q must NOT be in existed set, got %v", name, got)
		}
	}
}

// 5. dst doesn't exist (raw errs.ObjectNotFound) → empty map, no error.
//    THIS IS THE REGRESSION TEST FOR #1898.
func TestExistingDstFilesFn_DstDoesNotExist_RawError(t *testing.T) {
	rec := newListRecorder(func(string) ([]model.Obj, error) {
		return nil, errs.ObjectNotFound
	})
	got, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst/never-existed")
	if err != nil {
		t.Fatalf("ObjectNotFound on dst must be tolerated, got error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map on non-existent dst, got %v", got)
	}
}

// 6. dst doesn't exist (wrapped via pkg/errors.WithMessage) → empty map.
//    Guards the errors.Is unwrapping behavior that matters in practice:
//    op.List wraps the underlying ObjectNotFound with context messages.
func TestExistingDstFilesFn_DstDoesNotExist_WrappedError(t *testing.T) {
	rec := newListRecorder(func(string) ([]model.Obj, error) {
		// emulate the op.List wrapping path: GetUnwrap returns
		// ObjectNotFound, list wraps with WithMessage twice.
		return nil, pkgerrors.WithMessage(pkgerrors.WithMessage(errs.ObjectNotFound, "failed get dir"), "while listing")
	})
	got, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst")
	if err != nil {
		t.Fatalf("wrapped ObjectNotFound must be tolerated, got error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

// 7. List returns a non-ObjectNotFound error (e.g. permission denied,
//    I/O failure) → error propagates so the task fails fast.
func TestExistingDstFilesFn_OtherListError_Propagates(t *testing.T) {
	sentinel := errors.New("permission denied")
	rec := newListRecorder(func(string) ([]model.Obj, error) {
		return nil, sentinel
	})
	_, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst")
	if err == nil {
		t.Fatal("expected error to propagate, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel error, got: %v", err)
	}
}

// 8. Context cancelled before list → ctx.Err returned (the contract for
//    listDst is to honor ctx; the caller's loop also checks ctx.Err).
func TestExistingDstFilesFn_CtxCancelledBeforeList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := newListRecorder(func(string) ([]model.Obj, error) {
		// A real op.List would honor ctx and return ctx.Err.
		return nil, ctx.Err()
	})
	_, err := existingDstFilesFn(ctx, rec.listDst, "/dst")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

// 9. Context cancelled mid-iteration → loop bails with ctx.Err and the
//    partial map is not returned.
func TestExistingDstFilesFn_CtxCancelledDuringIteration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Big enough list that cancelling after returning still gives the
	// loop a chance to observe ctx.Err.
	objs := make([]model.Obj, 1000)
	for i := range objs {
		objs[i] = fileObj(fmt.Sprintf("f-%d.txt", i))
	}

	rec := newListRecorder(func(string) ([]model.Obj, error) {
		cancel() // cancel immediately after list returns
		return objs, nil
	})
	_, err := existingDstFilesFn(ctx, rec.listDst, "/dst")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

// 10. dst contains duplicate file names (pathological but possible if a
//     driver returns duplicates) → map dedupes silently, no panic, no error.
func TestExistingDstFilesFn_DuplicateNames(t *testing.T) {
	rec := newListRecorder(func(string) ([]model.Obj, error) {
		return []model.Obj{
			fileObj("dup.txt"),
			fileObj("dup.txt"),
			fileObj("unique.txt"),
		}, nil
	})
	got, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 dedup'd entries, got %d: %v", len(got), got)
	}
	if !got["dup.txt"] || !got["unique.txt"] {
		t.Fatalf("expected both names, got %v", got)
	}
}

// 11. dst contains a large number of files → all included; the call
//     completes within a reasonable budget (sanity, not strict perf).
func TestExistingDstFilesFn_LargeDst(t *testing.T) {
	const N = 10000
	objs := make([]model.Obj, N)
	for i := range objs {
		objs[i] = fileObj(fmt.Sprintf("f-%05d.bin", i))
	}
	rec := newListRecorder(func(string) ([]model.Obj, error) { return objs, nil })

	start := time.Now()
	got, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != N {
		t.Fatalf("expected %d entries, got %d", N, len(got))
	}
	// Generous bound — only catches catastrophic regressions (e.g.
	// accidental O(N²) inside the loop).
	if elapsed > 2*time.Second {
		t.Fatalf("processing %d entries took %v, too slow", N, elapsed)
	}
}

// shouldSkipForMerge mirrors the skip decision at copy_move.go:221 so the
// test below pins both the helper (existingDstFilesFn) AND the call-site
// guard as a single contract. Any future refactor that touches either
// half of this contract will be caught.
func shouldSkipForMerge(srcObj model.Obj, existedFiles map[string]bool) bool {
	return !srcObj.IsDir() && existedFiles[srcObj.GetName()]
}

// TestMergeSkipDecision_DirsAreNeverSkipped is the explicit anti-regression
// test for the worry "when a subdir exists in dst, the src subdir is
// skipped and its contents are not merged". It walks every (src kind, dst
// state) combination and asserts the skip decision.
//
// Why this matters: if existingDstFilesFn ever started including dirs, or
// if the call-site guard dropped the `!obj.IsDir()` clause, src subdirs
// matching dst subdirs would silently stop recursing — and every file
// inside them would never get copied. That bug is invisible to a casual
// "did the task complete?" check and only shows up as quietly missing
// deep files. Lock it down.
func TestMergeSkipDecision_DirsAreNeverSkipped(t *testing.T) {
	// dst already contains a mix: one file, two dirs.
	dstContents := []model.Obj{
		fileObj("root_file.txt"),
		dirObj("subA"),
		dirObj("subB"),
	}
	rec := newListRecorder(func(string) ([]model.Obj, error) { return dstContents, nil })

	existed, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Sanity: the dirs in dst must NOT be in existedObjs. If this fails,
	// every src dir matching a dst dir name would be skipped.
	if existed["subA"] || existed["subB"] {
		t.Fatalf("CRITICAL: dirs in dst leaked into existed-files map — "+
			"matching src subdirs would be skipped and lose their contents. "+
			"got %v", existed)
	}
	if !existed["root_file.txt"] {
		t.Fatalf("file in dst missing from existed map: %v", existed)
	}

	// Exhaustive skip-decision matrix for every src-object kind against
	// the populated existedObjs.
	cases := []struct {
		name     string
		srcObj   model.Obj
		wantSkip bool
		why      string
	}{
		{
			name:     "src_file_matches_dst_file",
			srcObj:   fileObj("root_file.txt"),
			wantSkip: true,
			why:      "resume semantics: already-uploaded file is skipped",
		},
		{
			name:     "src_dir_matches_dst_dir",
			srcObj:   dirObj("subA"),
			wantSkip: false,
			why:      "MUST recurse into matching subdir to merge its contents",
		},
		{
			name:     "src_dir_matches_dst_dir_B",
			srcObj:   dirObj("subB"),
			wantSkip: false,
			why:      "MUST recurse — every dst dir must trigger recursion regardless of name",
		},
		{
			name:     "src_dir_no_match",
			srcObj:   dirObj("brand_new_dir"),
			wantSkip: false,
			why:      "new dir → recurse and create",
		},
		{
			name:     "src_file_no_match",
			srcObj:   fileObj("brand_new_file.txt"),
			wantSkip: false,
			why:      "new file → upload",
		},
		{
			name:     "src_dir_matches_dst_FILE_name",
			srcObj:   dirObj("root_file.txt"),
			wantSkip: false,
			why:      "even with a name collision against a dst FILE, src dir must NOT be skipped " +
				"(the conflict will surface later when MakeDir runs, not silently)",
		},
		{
			name:     "src_file_matches_dst_DIR_name",
			srcObj:   fileObj("subA"),
			wantSkip: false,
			why:      "src file colliding with a dst dir name must NOT be skipped " +
				"(dirs are not in existed map, and op.Put will surface the conflict)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldSkipForMerge(c.srcObj, existed)
			if got != c.wantSkip {
				t.Fatalf("skip(src=%s,IsDir=%v) = %v, want %v — %s",
					c.srcObj.GetName(), c.srcObj.IsDir(), got, c.wantSkip, c.why)
			}
		})
	}
}

// TestMergeSkipDecision_EmptyDst: with no existedObjs, NOTHING is skipped
// regardless of obj kind — every src item gets a sub-task.
func TestMergeSkipDecision_EmptyDst(t *testing.T) {
	existed := map[string]bool{}
	for _, obj := range []model.Obj{
		fileObj("a.txt"),
		dirObj("d1"),
		fileObj("b.bin"),
		dirObj("d2"),
	} {
		if shouldSkipForMerge(obj, existed) {
			t.Errorf("nothing should be skipped against empty dst, but %s was", obj.GetName())
		}
	}
}

// TestMergeSkipDecision_DeepTreeRecursionContract simulates a 3-level src
// tree against a partial dst and asserts that the dir-recursion contract
// holds at every level. This is the "deep dir files missing" regression
// guard the user asked for.
func TestMergeSkipDecision_DeepTreeRecursionContract(t *testing.T) {
	// Three levels of nesting, with files at each level. Dst already has
	// the dir skeleton from a previous interrupted run, plus one file at
	// the deepest level (simulating partial completion).
	level0Src := []model.Obj{fileObj("root.txt"), dirObj("L1")}
	level1Src := []model.Obj{fileObj("a.txt"), dirObj("L2")}
	level2Src := []model.Obj{fileObj("deep1.txt"), fileObj("deep2.txt")}

	// Dst state per level
	level0Dst := []model.Obj{fileObj("root.txt"), dirObj("L1")}     // root.txt already uploaded
	level1Dst := []model.Obj{dirObj("L2")}                          // L1 exists, no files yet
	level2Dst := []model.Obj{fileObj("deep1.txt")}                  // deep1 already uploaded

	cases := []struct {
		level     string
		srcObjs   []model.Obj
		dstObjs   []model.Obj
		mustSkip  []string
		mustSpawn []string
	}{
		{
			level:     "L0",
			srcObjs:   level0Src,
			dstObjs:   level0Dst,
			mustSkip:  []string{"root.txt"}, // file already there
			mustSpawn: []string{"L1"},       // dir must recurse
		},
		{
			level:     "L1",
			srcObjs:   level1Src,
			dstObjs:   level1Dst,
			mustSkip:  []string{},                // a.txt not in dst
			mustSpawn: []string{"a.txt", "L2"},   // upload file + recurse dir
		},
		{
			level:     "L2",
			srcObjs:   level2Src,
			dstObjs:   level2Dst,
			mustSkip:  []string{"deep1.txt"}, // already uploaded
			mustSpawn: []string{"deep2.txt"}, // must still upload
		},
	}

	for _, c := range cases {
		t.Run(c.level, func(t *testing.T) {
			rec := newListRecorder(func(string) ([]model.Obj, error) { return c.dstObjs, nil })
			existed, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst/"+c.level)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var spawned, skipped []string
			for _, obj := range c.srcObjs {
				if shouldSkipForMerge(obj, existed) {
					skipped = append(skipped, obj.GetName())
				} else {
					spawned = append(spawned, obj.GetName())
				}
			}

			if !sameStrings(skipped, c.mustSkip) {
				t.Errorf("%s: skipped = %v, want %v", c.level, skipped, c.mustSkip)
			}
			if !sameStrings(spawned, c.mustSpawn) {
				t.Errorf("%s: spawned = %v, want %v", c.level, spawned, c.mustSpawn)
			}
		})
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

// 12. Sanity: the helper invokes listDst exactly once with the given
//     dstPath (no double-listing).
func TestExistingDstFilesFn_CallsListOnce(t *testing.T) {
	rec := newListRecorder(func(string) ([]model.Obj, error) {
		return []model.Obj{fileObj("a.txt")}, nil
	})
	_, err := existingDstFilesFn(context.Background(), rec.listDst, "/dst/exact/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.calls) != 1 || rec.calls[0] != "/dst/exact/path" {
		t.Fatalf("expected single call to /dst/exact/path, got %v", rec.calls)
	}
}
