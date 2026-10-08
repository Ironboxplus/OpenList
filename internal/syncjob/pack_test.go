package syncjob_test

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/syncjob"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
)

type testManifest struct {
	Version int `json:"version"`
	Files   map[string]struct {
		Size int64  `json:"size"`
		Pack string `json:"pack"`
		SHA1 string `json:"sha1"`
	} `json:"files"`
	Packs map[string]struct {
		Bytes int64 `json:"bytes"`
		Live  int   `json:"live"`
		Dead  int   `json:"dead"`
	} `json:"packs"`
}

func loadTestManifest(t *testing.T, root, dir string) testManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), ".oppack", "index.json"))
	if err != nil {
		t.Fatalf("manifest of %q: %v", dir, err)
	}
	var m testManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

func setMTime(t *testing.T, root, rel string, mt time.Time) {
	t.Helper()
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(rel)), mt, mt); err != nil {
		t.Fatal(err)
	}
}

func fileNames(m testManifest) []string {
	var out []string
	for n := range m.Files {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func packSpec(mode syncjob.Mode, src, dst string, p syncjob.PackOptions) syncjob.Spec {
	p.Enabled = true
	return syncjob.Spec{Mode: mode, Src: "local:" + src, Dst: "local:" + dst, Pack: &p}
}

func mustSucceed(t *testing.T, s syncjob.Snapshot) syncjob.Snapshot {
	t.Helper()
	if s.State != syncjob.StateSucceeded {
		t.Fatalf("job %s: %s", s.State, s.Error)
	}
	return s
}

func TestPackLayoutAndCompression(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	for i := 0; i < 6; i++ {
		write(t, src, fmt.Sprintf("d/a%d.txt", i), strings.Repeat("text ", 40))
	}
	write(t, src, "d/w.pt", strings.Repeat("weights", 20))
	write(t, src, "d/big.bin", strings.Repeat("B", 5000))
	write(t, src, "d/sub/s1.txt", "one")
	write(t, src, "d/sub/s2.txt", "two")
	for i := 0; i < 3; i++ {
		write(t, src, fmt.Sprintf("r%d.txt", i), "root")
	}

	s := mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, syncjob.PackOptions{Threshold: 1024, MinFiles: 3})))
	if s.PackStats == nil || s.PackStats.Dirs != 2 || s.PackStats.PackedFiles != 10 || s.PackStats.Packs != 2 || s.PackStats.SkippedDirs != 1 {
		t.Fatalf("pack stats = %+v", s.PackStats)
	}
	if s.Stage != "transferring" {
		t.Fatalf("stage = %q", s.Stage)
	}

	mf := loadTestManifest(t, dst, "d")
	if got := strings.Join(fileNames(mf), ","); got != "a0.txt,a1.txt,a2.txt,a3.txt,a4.txt,a5.txt,w.pt" {
		t.Fatalf("packed files = %s", got)
	}
	if exists(dst, "d/a0.txt") || exists(dst, "d/w.pt") || exists(dst, "r0.txt") {
		t.Fatalf("a packed file was also uploaded loose")
	}
	if !exists(dst, "d/big.bin") || !exists(dst, "d/sub/s1.txt") || !exists(dst, "d/sub/s2.txt") {
		t.Fatalf("big file or the below-MinFiles directory was not transferred as usual")
	}
	if exists(dst, "d/sub/.oppack") {
		t.Fatalf("a directory under MinFiles got a pack")
	}
	if !exists(dst, ".oppack/index.json") {
		t.Fatalf("root directory not packed")
	}

	zr, err := zip.OpenReader(filepath.Join(dst, "d", ".oppack", mf.Files["a0.txt"].Pack))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	methods := map[string]uint16{}
	for _, f := range zr.File {
		methods[f.Name] = f.Method
	}
	if methods["w.pt"] != zip.Store || methods["a0.txt"] != zip.Deflate {
		t.Fatalf("methods = %v, want .pt stored and .txt deflated", methods)
	}
}

func TestPackThresholds(t *testing.T) {
	src := tempDir(t)
	for _, sz := range []int{500, 2048, 20 << 10} {
		for i := 0; i < 3; i++ {
			write(t, src, fmt.Sprintf("d/f%d-%d.dat", sz, i), strings.Repeat("x", sz))
		}
	}
	for _, c := range []struct {
		threshold int64
		want      int
	}{{1024, 3}, {4096, 6}, {64 << 10, 9}} {
		dst := tempDir(t)
		mustSucceed(t, runJob(t, newManager(t), packSpec(syncjob.ModeCopy, src, dst, syncjob.PackOptions{Threshold: c.threshold, MinFiles: 3})))
		if got := len(loadTestManifest(t, dst, "d").Files); got != c.want {
			t.Errorf("threshold %d packed %d files, want %d", c.threshold, got, c.want)
		}
	}
}

func TestPackIncrementalDeadCountsAndCompaction(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 8; i++ {
		rel := fmt.Sprintf("d/f%d.txt", i)
		write(t, src, rel, fmt.Sprintf("content %d", i))
		setMTime(t, src, rel, old)
	}
	opts := syncjob.PackOptions{Threshold: 1024, MinFiles: 3}

	s := mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	if s.PackStats.Packs != 1 || s.PackStats.PackedFiles != 8 {
		t.Fatalf("first run stats = %+v", s.PackStats)
	}

	// Nothing changed: nothing is packed again.
	s = mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	if s.PackStats.Packs != 0 || s.PackStats.PackedFiles != 0 {
		t.Fatalf("unchanged run repacked: %+v", s.PackStats)
	}

	// One edit makes exactly one new pack holding exactly that file.
	write(t, src, "d/f3.txt", "content three, edited")
	s = mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	if s.PackStats.Packs != 1 || s.PackStats.PackedFiles != 1 || s.PackStats.DeadRemoved != 1 {
		t.Fatalf("edit run stats = %+v", s.PackStats)
	}
	mf := loadTestManifest(t, dst, "d")
	if mf.Files["f3.txt"].Pack != "p-000002.zip" || mf.Files["f0.txt"].Pack != "p-000001.zip" {
		t.Fatalf("manifest after edit = %+v", mf.Files)
	}
	if p := mf.Packs["p-000001.zip"]; p.Live != 7 || p.Dead != 1 {
		t.Fatalf("p-000001 live/dead = %d/%d, want 7/1", p.Live, p.Dead)
	}

	// Deleting at src is only followed in sync mode; copy keeps the entry.
	if err := os.Remove(filepath.Join(src, "d", "f0.txt")); err != nil {
		t.Fatal(err)
	}
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, opts)))
	if _, ok := loadTestManifest(t, dst, "d").Files["f0.txt"]; !ok {
		t.Fatalf("copy mode dropped a deleted file from the manifest")
	}
	s = mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	mf = loadTestManifest(t, dst, "d")
	if _, ok := mf.Files["f0.txt"]; ok || s.PackStats.DeadRemoved != 1 {
		t.Fatalf("sync mode kept f0.txt (stats %+v)", s.PackStats)
	}
	if p := mf.Packs["p-000001.zip"]; p.Live != 6 || p.Dead != 2 {
		t.Fatalf("p-000001 live/dead = %d/%d, want 6/2", p.Live, p.Dead)
	}

	// Delete more until dead outnumber live: the pack is rewritten and the
	// old one removed from dst.
	for _, n := range []string{"f1", "f2", "f4", "f5"} {
		_ = os.Remove(filepath.Join(src, "d", n+".txt"))
	}
	s = mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	mf = loadTestManifest(t, dst, "d")
	if exists(dst, "d/.oppack/p-000001.zip") {
		t.Fatalf("compacted pack still at dst")
	}
	if got := strings.Join(fileNames(mf), ","); got != "f3.txt,f6.txt,f7.txt" {
		t.Fatalf("files after compaction = %s", got)
	}
	if len(mf.Packs) != 2 {
		t.Fatalf("packs after compaction = %+v", mf.Packs)
	}
	for name, p := range mf.Packs {
		if p.Live == 0 || p.Dead != 0 {
			t.Fatalf("pack %s live/dead = %d/%d", name, p.Live, p.Dead)
		}
		if !exists(dst, "d/.oppack/"+name) {
			t.Fatalf("pack %s listed but missing at dst", name)
		}
	}
}

func TestPackRollsOverAtMaxPackBytes(t *testing.T) {
	src, dst := tempDir(t), tempDir(t)
	for i := 0; i < 12; i++ {
		write(t, src, fmt.Sprintf("d/f%02d.bin", i), strings.Repeat(string(rune('a'+i)), 4000))
	}
	s := mustSucceed(t, runJob(t, newManager(t), packSpec(syncjob.ModeCopy, src, dst,
		syncjob.PackOptions{Threshold: 8192, MinFiles: 3, MaxPackBytes: 10000})))
	if s.PackStats.Packs < 4 {
		t.Fatalf("12x4000 bytes with a 10000 byte cap made %d packs", s.PackStats.Packs)
	}
	if got := len(loadTestManifest(t, dst, "d").Files); got != 12 {
		t.Fatalf("manifest lists %d files", got)
	}
}

func TestPackSyncKeepsOppackAndRemovesStrays(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	for i := 0; i < 4; i++ {
		write(t, src, fmt.Sprintf("d/f%d.txt", i), "small")
	}
	write(t, dst, "d/stray.txt", "only at dst")
	write(t, dst, "gone/old.txt", "whole directory only at dst")
	opts := syncjob.PackOptions{Threshold: 1024, MinFiles: 3}
	for i := 0; i < 2; i++ {
		mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
		if !exists(dst, "d/.oppack/index.json") {
			t.Fatalf("run %d: sync deleted the manifest", i)
		}
		if exists(dst, "d/stray.txt") || exists(dst, "gone/old.txt") {
			t.Fatalf("run %d: sync left files that are not at src", i)
		}
		if exists(dst, "d/f0.txt") {
			t.Fatalf("run %d: packed file uploaded loose", i)
		}
	}
}

func TestPackDryRunWritesNothing(t *testing.T) {
	src, dst := tempDir(t), tempDir(t)
	for i := 0; i < 4; i++ {
		write(t, src, fmt.Sprintf("d/f%d.txt", i), "small")
	}
	s := mustSucceed(t, runJob(t, newManager(t), func() syncjob.Spec {
		sp := packSpec(syncjob.ModeCopy, src, dst, syncjob.PackOptions{Threshold: 1024, MinFiles: 3})
		sp.DryRun = true
		return sp
	}()))
	if s.PackStats.PackedFiles != 4 {
		t.Fatalf("dry run stats = %+v", s.PackStats)
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Fatalf("dry run wrote %d entries to dst", len(entries))
	}
}

func TestPackOnlyForCopyAndSync(t *testing.T) {
	m := newManager(t)
	mountLocal(t, "/job-pack-bad")
	for _, mode := range []syncjob.Mode{syncjob.ModeBisync, syncjob.ModeCheck} {
		spec := syncjob.Spec{Mode: mode, Src: "/job-pack-bad", Dst: "/job-pack-bad", Pack: &syncjob.PackOptions{Enabled: true}}
		if _, err := m.Start(spec); err == nil {
			t.Errorf("pack accepted with %s", mode)
		}
	}
	bad := syncjob.Spec{Mode: syncjob.ModeCopy, Src: "/job-pack-bad", Dst: "/job-pack-bad", Pack: &syncjob.PackOptions{Enabled: true, Threshold: -1}}
	if _, err := m.Start(bad); err == nil {
		t.Errorf("negative threshold accepted")
	}
}

func TestPackDefaultsAreInOnePlace(t *testing.T) {
	d := syncjob.DefaultPackOptions()
	if d.Threshold != 8<<20 || d.MinFiles != 8 || d.MaxPackBytes != 1<<30 || len(d.StoreSuffixes) < 20 {
		t.Fatalf("defaults = %+v", d)
	}
	// Unset fields in a request mean those defaults.
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	for i := 0; i < 8; i++ {
		write(t, src, fmt.Sprintf("f%d.txt", i), "x")
	}
	mustSucceed(t, runJob(t, m, syncjob.Spec{Mode: syncjob.ModeCopy, Src: "local:" + src, Dst: "local:" + dst, Pack: &syncjob.PackOptions{Enabled: true}}))
	if !exists(dst, ".oppack/index.json") {
		t.Fatalf("8 small files with default options were not packed")
	}
}

func TestPackIsStickyBelowMinFiles(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	for i := 0; i < 5; i++ {
		write(t, src, fmt.Sprintf("d/f%d.txt", i), "small")
	}
	opts := syncjob.PackOptions{Threshold: 1024, MinFiles: 4}
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))

	// Down to 2 small files (< MinFiles): still packed, nothing loose.
	for i := 2; i < 5; i++ {
		_ = os.Remove(filepath.Join(src, "d", fmt.Sprintf("f%d.txt", i)))
	}
	write(t, src, "d/f0.txt", "edited")
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	if exists(dst, "d/f0.txt") || exists(dst, "d/f1.txt") {
		t.Fatalf("small files went up loose next to a manifest")
	}
	if got := strings.Join(fileNames(loadTestManifest(t, dst, "d")), ","); got != "f0.txt,f1.txt" {
		t.Fatalf("manifest = %s", got)
	}

	// Down to none: the whole .oppack goes.
	_ = os.Remove(filepath.Join(src, "d", "f0.txt"))
	_ = os.Remove(filepath.Join(src, "d", "f1.txt"))
	write(t, src, "d/big.bin", strings.Repeat("B", 3000))
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	if exists(dst, "d/.oppack") {
		t.Fatalf(".oppack still at dst with no live files")
	}
	if !exists(dst, "d/big.bin") {
		t.Fatalf("big file missing")
	}
}

func TestPackOrphanOppackFollowsSyncOnly(t *testing.T) {
	opts := syncjob.PackOptions{Threshold: 1024, MinFiles: 3}
	for _, mode := range []syncjob.Mode{syncjob.ModeSync, syncjob.ModeCopy} {
		m := newManager(t)
		src, dst := tempDir(t), tempDir(t)
		for i := 0; i < 4; i++ {
			write(t, src, fmt.Sprintf("gone/sub/f%d.txt", i), "small")
			write(t, src, fmt.Sprintf("keep/f%d.txt", i), "small")
		}
		mustSucceed(t, runJob(t, m, packSpec(mode, src, dst, opts)))
		if !exists(dst, "gone/sub/.oppack/index.json") {
			t.Fatalf("%s: setup: no pack", mode)
		}
		if err := os.RemoveAll(filepath.Join(src, "gone")); err != nil {
			t.Fatal(err)
		}
		mustSucceed(t, runJob(t, m, packSpec(mode, src, dst, opts)))
		if mode == syncjob.ModeSync {
			if exists(dst, "gone") {
				t.Fatalf("sync left the deleted directory (or its .oppack) at dst")
			}
		} else if !exists(dst, "gone/sub/.oppack/index.json") {
			t.Fatalf("copy removed .oppack of a deleted directory")
		}
		if !exists(dst, "keep/.oppack/index.json") {
			t.Fatalf("%s: unrelated .oppack removed", mode)
		}
	}
}

// Finding out whether dst has a pack must not cost a driver Get per directory
// (115 pays a rate-limited call for each): a pack-mode run over a tree with no
// packs makes no more Gets than a plain run.
func TestPackDetectionCostsNoExtraDriverGets(t *testing.T) {
	m := newManager(t)
	src := tempDir(t)
	for d := 0; d < 12; d++ {
		for f := 0; f < 3; f++ {
			write(t, src, fmt.Sprintf("d%02d/s%d/f%d.txt", d, d%3, f), "x")
		}
	}
	newMount := func(mount string) *fakedriver.Driver {
		id, err := op.CreateStorage(context.Background(), model.Storage{Driver: fakedriver.DriverName, MountPath: mount, Addition: "{}", CacheExpiration: 30})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
		s, _ := op.GetStorageByMountPath(mount)
		return s.(*fakedriver.Driver)
	}
	plainDrv, packDrv := newMount("/fk-plain"), newMount("/fk-pack")
	spec := func(dst string, pack bool) syncjob.Spec {
		sp := syncjob.Spec{Mode: syncjob.ModeSync, Src: "local:" + src, Dst: dst}
		if pack {
			sp.Pack = &syncjob.PackOptions{Enabled: true, MinFiles: 8}
		}
		return sp
	}
	// Both destinations already hold the tree (no writes, so no hooks or
	// uploads move the counts) and start with a cold dir cache.
	for _, drv := range []*fakedriver.Driver{plainDrv, packDrv} {
		for d := 0; d < 12; d++ {
			drv.AddDir(fmt.Sprintf("/d%02d", d))
			drv.AddDir(fmt.Sprintf("/d%02d/s%d", d, d%3))
			for f := 0; f < 3; f++ {
				drv.AddFile(fmt.Sprintf("/d%02d/s%d/f%d.txt", d, d%3, f), "x")
			}
		}
	}
	count := func(drv *fakedriver.Driver, dst string, pack bool) int32 {
		op.Cache.ClearAll()
		drv.ResetCounts()
		s := mustSucceed(t, runJob(t, m, spec(dst, pack)))
		if s.Counters.Match == 0 || s.Counters.OnlyInSrc != 0 || s.Counters.Uploaded != 0 {
			t.Fatalf("dst should already match src: %+v", s.Counters)
		}
		if pack && s.PackStats.Packs != 0 {
			t.Fatalf("test tree was packed: %+v", s.PackStats)
		}
		return drv.Gets.Load()
	}
	plain := count(plainDrv, "/fk-plain", false)
	packed := count(packDrv, "/fk-pack", true)
	t.Logf("driver Gets: plain=%d pack=%d", plain, packed)
	if packed > plain {
		t.Fatalf("pack mode made %d driver Gets, plain run %d", packed, plain)
	}
}

// tempDir is t.TempDir with a cleanup that retries: on Windows removal of a
// just-written directory tree intermittently fails with "directory not
// empty" while the file system lets go of the files.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "syncjob-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var err error
		for i := 0; i < 20; i++ {
			if err = os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Errorf("cleanup %s: %v", dir, err)
	})
	return dir
}

func TestPackRemovesSupersededLooseCopiesAndKeepsConflicts(t *testing.T) {
	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, mode := range []syncjob.Mode{syncjob.ModeCopy, syncjob.ModeSync} {
		m := newManager(t)
		src, dst := tempDir(t), tempDir(t)
		for i := 0; i < 5; i++ {
			rel := fmt.Sprintf("d/f%d.txt", i)
			write(t, src, rel, "content")
			setMTime(t, src, rel, when)
			// An earlier non-pack run left the same file loose at dst.
			write(t, dst, rel, "content")
			setMTime(t, dst, rel, when)
		}
		// f3: dst copy has other content; f4: same content, other time.
		write(t, dst, "d/f3.txt", "different size!")
		setMTime(t, dst, "d/f4.txt", when.Add(time.Hour))

		s := mustSucceed(t, runJob(t, m, packSpec(mode, src, dst, syncjob.PackOptions{Threshold: 1024, MinFiles: 3})))
		for _, gone := range []string{"f0", "f1", "f2"} {
			if exists(dst, "d/"+gone+".txt") {
				t.Fatalf("%s: superseded loose copy %s kept", mode, gone)
			}
		}
		for _, kept := range []string{"f3", "f4"} {
			if !exists(dst, "d/"+kept+".txt") {
				t.Fatalf("%s: conflicting loose copy %s deleted", mode, kept)
			}
		}
		if got, _ := read(t, dst, "d/f3.txt"); got != "different size!" {
			t.Fatalf("%s: conflicting file content changed: %q", mode, got)
		}
		if s.PackStats.Conflicts != 2 {
			t.Fatalf("%s: conflicts = %d, want 2", mode, s.PackStats.Conflicts)
		}
		if len(loadTestManifest(t, dst, "d").Files) != 5 {
			t.Fatalf("%s: not all files packed", mode)
		}
	}
}
