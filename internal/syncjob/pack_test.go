package syncjob_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
	Next    int `json:"next"`
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

func readTestManifest(t *testing.T, root, name string) testManifest {
	t.Helper()
	f, err := os.Open(filepath.Join(root, ".oppack", name))
	if err != nil {
		t.Fatalf("manifest %s: %v", name, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("manifest %s is not gzip: %v", name, err)
	}
	var m testManifest
	if err := json.NewDecoder(zr).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// loadTestManifest reads the job manifest at root.
func loadTestManifest(t *testing.T, root string) testManifest {
	t.Helper()
	m := readTestManifest(t, root, "manifest.json.gz")
	if m.Version != 2 {
		t.Fatalf("manifest version %d", m.Version)
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

// looseSmall lists files under root, outside .oppack, smaller than threshold.
func looseSmall(t *testing.T, root string, threshold int64) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".oppack" {
				return filepath.SkipDir
			}
			return nil
		}
		if info, _ := d.Info(); info.Size() < threshold {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func packNames(t *testing.T, root string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(root, ".oppack"))
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zip") {
			out = append(out, e.Name())
		}
	}
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
	// A directory with a single small file is packed like any other.
	write(t, src, "d/sub/s1.txt", "one")
	write(t, src, "lonely/only.txt", "alone")
	write(t, src, "r0.txt", "root")

	s := mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, syncjob.PackOptions{Threshold: 1024})))
	if st := s.PackStats; st == nil || st.SmallFiles != 10 || st.PackedFiles != 10 || st.PacksWritten != 1 || st.PacksTotal != 1 {
		t.Fatalf("pack stats = %+v", s.PackStats)
	}
	if s.Stage != "transferring" {
		t.Fatalf("stage = %q", s.Stage)
	}
	if loose := looseSmall(t, dst, 1024); len(loose) != 0 {
		t.Fatalf("small files uploaded on their own: %v", loose)
	}
	if !exists(dst, "d/big.bin") {
		t.Fatalf("large file not transferred as usual")
	}
	for _, d := range []string{"d/.oppack", "d/sub/.oppack", "lonely/.oppack"} {
		if exists(dst, d) {
			t.Fatalf("per-directory %s created", d)
		}
	}

	mf := loadTestManifest(t, dst)
	want := "d/a0.txt,d/a1.txt,d/a2.txt,d/a3.txt,d/a4.txt,d/a5.txt,d/sub/s1.txt,d/w.pt,lonely/only.txt,r0.txt"
	if got := strings.Join(fileNames(mf), ","); got != want {
		t.Fatalf("manifest files = %s", got)
	}

	zr, err := zip.OpenReader(filepath.Join(dst, ".oppack", "p-000001.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	methods := map[string]uint16{}
	for _, f := range zr.File {
		methods[f.Name] = f.Method
	}
	if len(methods) != 10 || methods["d/w.pt"] != zip.Store || methods["d/a0.txt"] != zip.Deflate {
		t.Fatalf("members/methods = %v, want full paths, .pt stored and .txt deflated", methods)
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
		mustSucceed(t, runJob(t, newManager(t), packSpec(syncjob.ModeCopy, src, dst, syncjob.PackOptions{Threshold: c.threshold})))
		if got := len(loadTestManifest(t, dst).Files); got != c.want {
			t.Errorf("threshold %d packed %d files, want %d", c.threshold, got, c.want)
		}
		if loose := looseSmall(t, dst, c.threshold); len(loose) != 0 {
			t.Errorf("threshold %d: loose small files %v", c.threshold, loose)
		}
	}
}

func TestPackIncrementalDeadCountsAndCompaction(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 8; i++ {
		rel := fmt.Sprintf("d%d/f.txt", i)
		write(t, src, rel, fmt.Sprintf("content %d", i))
		setMTime(t, src, rel, old)
	}
	opts := syncjob.PackOptions{Threshold: 1024}

	s := mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	if s.PackStats.PacksWritten != 1 || s.PackStats.PackedFiles != 8 {
		t.Fatalf("first run stats = %+v", s.PackStats)
	}

	// Nothing changed: nothing is packed or committed again.
	before := readTestManifest(t, dst, "manifest.json.gz")
	s = mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	if s.PackStats.PacksWritten != 0 || s.PackStats.PackedFiles != 0 || s.PackStats.PacksTotal != 1 {
		t.Fatalf("unchanged run repacked: %+v", s.PackStats)
	}
	if exists(dst, ".oppack/manifest.prev.json.gz") {
		t.Fatalf("an unchanged run committed a new manifest")
	}

	// One edit makes exactly one new pack holding exactly that file, and one
	// new manifest; the old one is kept as prev.
	write(t, src, "d3/f.txt", "content three, edited")
	s = mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	if s.PackStats.PacksWritten != 1 || s.PackStats.PackedFiles != 1 || s.PackStats.DeadRemoved != 1 || s.PackStats.PacksTotal != 2 {
		t.Fatalf("edit run stats = %+v", s.PackStats)
	}
	mf := loadTestManifest(t, dst)
	if mf.Files["d3/f.txt"].Pack != "p-000002.zip" || mf.Files["d0/f.txt"].Pack != "p-000001.zip" {
		t.Fatalf("manifest after edit = %+v", mf.Files)
	}
	if p := mf.Packs["p-000001.zip"]; p.Live != 7 || p.Dead != 1 {
		t.Fatalf("p-000001 live/dead = %d/%d, want 7/1", p.Live, p.Dead)
	}
	prev := readTestManifest(t, dst, "manifest.prev.json.gz")
	if len(prev.Files) != len(before.Files) || prev.Files["d3/f.txt"].Pack != "p-000001.zip" {
		t.Fatalf("manifest.prev is not the manifest before the edit: %+v", prev.Files)
	}

	// Deleting at src is only followed in sync mode; copy keeps the entry.
	if err := os.RemoveAll(filepath.Join(src, "d0")); err != nil {
		t.Fatal(err)
	}
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, opts)))
	if _, ok := loadTestManifest(t, dst).Files["d0/f.txt"]; !ok {
		t.Fatalf("copy mode dropped a deleted file from the manifest")
	}
	s = mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	mf = loadTestManifest(t, dst)
	if _, ok := mf.Files["d0/f.txt"]; ok || s.PackStats.DeadRemoved != 1 {
		t.Fatalf("sync mode kept d0/f.txt (stats %+v)", s.PackStats)
	}
	if p := mf.Packs["p-000001.zip"]; p.Live != 6 || p.Dead != 2 {
		t.Fatalf("p-000001 live/dead = %d/%d, want 6/2", p.Live, p.Dead)
	}

	// Delete more until dead outnumber live: the pack is rewritten and the
	// old one removed from dst.
	for _, n := range []string{"d1", "d2", "d4", "d5"} {
		_ = os.RemoveAll(filepath.Join(src, n))
	}
	s = mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	mf = loadTestManifest(t, dst)
	if exists(dst, ".oppack/p-000001.zip") {
		t.Fatalf("compacted pack still at dst")
	}
	if got := strings.Join(fileNames(mf), ","); got != "d3/f.txt,d6/f.txt,d7/f.txt" {
		t.Fatalf("files after compaction = %s", got)
	}
	if s.PackStats.CompactedPacks != 1 {
		t.Fatalf("compacted packs = %d", s.PackStats.CompactedPacks)
	}
	for name, p := range mf.Packs {
		if p.Live == 0 || p.Dead != 0 {
			t.Fatalf("pack %s live/dead = %d/%d", name, p.Live, p.Dead)
		}
		if !exists(dst, ".oppack/"+name) {
			t.Fatalf("pack %s listed but missing at dst", name)
		}
	}
	if got := len(packNames(t, dst)); got != len(mf.Packs) {
		t.Fatalf("%d packs at dst, %d in manifest", got, len(mf.Packs))
	}
}

func TestPackRemovedEntirelyRemovesOppack(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	write(t, src, "a/x.txt", "x")
	write(t, src, "big.bin", strings.Repeat("B", 3000))
	opts := syncjob.PackOptions{Threshold: 1024}
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	if err := os.RemoveAll(filepath.Join(src, "a")); err != nil {
		t.Fatal(err)
	}
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
	if exists(dst, ".oppack") {
		t.Fatalf(".oppack still at dst with no live files")
	}
	if !exists(dst, "big.bin") || exists(dst, "a") {
		t.Fatalf("dst tree wrong after sync")
	}
}

func TestPackRollsOverAtMaxPackBytes(t *testing.T) {
	src, dst := tempDir(t), tempDir(t)
	for i := 0; i < 12; i++ {
		write(t, src, fmt.Sprintf("d/f%02d.bin", i), strings.Repeat(string(rune('a'+i)), 4000))
	}
	s := mustSucceed(t, runJob(t, newManager(t), packSpec(syncjob.ModeCopy, src, dst,
		syncjob.PackOptions{Threshold: 8192, MaxPackBytes: 10000})))
	if s.PackStats.PacksWritten < 4 {
		t.Fatalf("12x4000 bytes with a 10000 byte cap made %d packs", s.PackStats.PacksWritten)
	}
	if got := len(loadTestManifest(t, dst).Files); got != 12 {
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
	opts := syncjob.PackOptions{Threshold: 1024}
	for i := 0; i < 2; i++ {
		mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, opts)))
		if !exists(dst, ".oppack/manifest.json.gz") {
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
	sp := packSpec(syncjob.ModeCopy, src, dst, syncjob.PackOptions{Threshold: 1024})
	sp.DryRun = true
	s := mustSucceed(t, runJob(t, newManager(t), sp))
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
	if d.Threshold != 8<<20 || d.MaxPackBytes != 1<<30 || len(d.StoreSuffixes) < 20 {
		t.Fatalf("defaults = %+v", d)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "min_files") {
		t.Fatalf("min_files is still an option: %s", b)
	}
	// Unset fields in a request mean those defaults, and even a lone small
	// file is packed.
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	write(t, src, "f.txt", "x")
	mustSucceed(t, runJob(t, m, syncjob.Spec{Mode: syncjob.ModeCopy, Src: "local:" + src, Dst: "local:" + dst, Pack: &syncjob.PackOptions{Enabled: true}}))
	if !exists(dst, ".oppack/manifest.json.gz") || exists(dst, "f.txt") {
		t.Fatalf("a lone small file with default options was not packed")
	}
}

// A failure while packs go up must leave the previous manifest and packs
// untouched, and the next run must carry on correctly.
func TestPackFailureBeforeCommitKeepsOldManifest(t *testing.T) {
	m := newManager(t)
	src := tempDir(t)
	for i := 0; i < 4; i++ {
		write(t, src, fmt.Sprintf("d/f%d.txt", i), fmt.Sprintf("v1 %d", i))
	}
	drv := fakeMount(t, "/fk-atomic")
	spec := syncjob.Spec{Mode: syncjob.ModeSync, Src: "local:" + src, Dst: "/fk-atomic",
		Pack: &syncjob.PackOptions{Enabled: true, Threshold: 1024, MaxPackBytes: 1}}
	mustSucceed(t, runJob(t, m, spec))
	time.Sleep(200 * time.Millisecond)
	manifestBefore, ok := drv.Content("/.oppack/manifest.json.gz")
	if !ok {
		t.Fatal("no manifest after the first run")
	}
	packBefore, _ := drv.Content("/.oppack/p-000001.zip")

	write(t, src, "d/f1.txt", "v2 edited")
	write(t, src, "d/new.txt", "brand new")
	// Packs go up one per file; the second one fails.
	var puts int
	drv.FailPut = func(path string) error {
		if strings.HasPrefix(path, "/.oppack/p-") {
			if puts++; puts == 2 {
				return fmt.Errorf("injected failure on %s", path)
			}
		}
		return nil
	}
	s := runJob(t, m, spec)
	if s.State != syncjob.StateFailed || !strings.Contains(s.Error, "injected failure") {
		t.Fatalf("job state = %s (%s), want failed by the injected error", s.State, s.Error)
	}
	if after, _ := drv.Content("/.oppack/manifest.json.gz"); !bytes.Equal(manifestBefore, after) {
		t.Fatalf("manifest changed although the run failed before the commit")
	}
	if _, there := drv.Content("/.oppack/manifest.prev.json.gz"); there {
		t.Fatalf("manifest.prev written before the commit")
	}
	if after, _ := drv.Content("/.oppack/p-000001.zip"); !bytes.Equal(packBefore, after) {
		t.Fatalf("an existing pack was modified")
	}

	// Next run, nothing failing: carries on and commits, never reusing the
	// names of the orphan packs the failed run left.
	drv.FailPut = nil
	s = mustSucceed(t, runJob(t, m, spec))
	time.Sleep(200 * time.Millisecond)
	raw, _ := drv.Content("/.oppack/manifest.json.gz")
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var mf testManifest
	if err := json.NewDecoder(zr).Decode(&mf); err != nil {
		t.Fatal(err)
	}
	if mf.Files["d/f1.txt"].Pack == "p-000001.zip" || mf.Files["d/new.txt"].Pack == "" {
		t.Fatalf("retry did not pack the changes: %+v", mf.Files)
	}
	for name := range mf.Packs {
		if _, ok := drv.Content("/.oppack/" + name); !ok {
			t.Fatalf("manifest lists %s which is not at dst", name)
		}
	}
	if _, ok := drv.Content("/.oppack/manifest.prev.json.gz"); !ok {
		t.Fatalf("retry did not keep the previous manifest")
	}
	if s.PackStats.PackedFiles != 2 {
		t.Fatalf("retry stats = %+v", s.PackStats)
	}
}

// A lost manifest must not let new packs overwrite packs that are still there.
func TestPackNeverReusesAPackName(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	write(t, src, "a/x.txt", "x")
	opts := syncjob.PackOptions{Threshold: 1024}
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, opts)))
	_ = os.Remove(filepath.Join(dst, ".oppack", "manifest.json.gz"))
	_ = os.Remove(filepath.Join(dst, ".oppack", "manifest.prev.json.gz"))
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, opts)))
	if got := loadTestManifest(t, dst).Files["a/x.txt"].Pack; got != "p-000002.zip" {
		t.Fatalf("a/x.txt in %s, want a fresh p-000002.zip", got)
	}
	// The old pack was not reused; with no manifest left to refer to it, it is
	// an orphan and goes once the new manifest is committed.
	if exists(dst, ".oppack/p-000001.zip") {
		t.Fatalf("orphan pack kept")
	}
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

		s := mustSucceed(t, runJob(t, m, packSpec(mode, src, dst, syncjob.PackOptions{Threshold: 1024})))
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
		if s.PackStats.Conflicts != 2 || s.PackStats.LooseRemoved != 3 {
			t.Fatalf("%s: conflicts/loose_removed = %d/%d, want 2/3", mode, s.PackStats.Conflicts, s.PackStats.LooseRemoved)
		}
		if len(loadTestManifest(t, dst).Files) != 5 {
			t.Fatalf("%s: not all files packed", mode)
		}
	}
}

func fakeMount(t *testing.T, mount string) *fakedriver.Driver {
	t.Helper()
	id, err := op.CreateStorage(context.Background(), model.Storage{Driver: fakedriver.DriverName, MountPath: mount, Addition: "{}", CacheExpiration: 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	s, _ := op.GetStorageByMountPath(mount)
	return s.(*fakedriver.Driver)
}

// The point of job-level packs: with one small file in each of N directories
// the uploads are the packs plus the manifest, however large N is.
func TestPackUploadCountDoesNotGrowWithDirectories(t *testing.T) {
	m := newManager(t)
	var puts []int32
	for i, n := range []int{10, 40} {
		src := tempDir(t)
		for d := 0; d < n; d++ {
			write(t, src, fmt.Sprintf("dir%03d/only.json", d), fmt.Sprintf(`{"d":%d}`, d))
		}
		mount := fmt.Sprintf("/fk-cost%d", i)
		drv := fakeMount(t, mount)
		drv.ResetCounts()
		s := mustSucceed(t, runJob(t, m, syncjob.Spec{Mode: syncjob.ModeCopy, Src: "local:" + src, Dst: mount,
			Pack: &syncjob.PackOptions{Enabled: true}}))
		time.Sleep(200 * time.Millisecond)
		if s.PackStats.PacksWritten != 1 || s.PackStats.PackedFiles != int64(n) {
			t.Fatalf("N=%d: pack stats %+v", n, s.PackStats)
		}
		got := drv.Puts.Load()
		t.Logf("N=%d dirs: %d uploads", n, got)
		if want := int32(s.PackStats.PacksWritten) + 1; got != want {
			t.Fatalf("N=%d: %d uploads, want packs+1 = %d", n, got, want)
		}
		puts = append(puts, got)
	}
	if puts[0] != puts[1] {
		t.Fatalf("uploads grew with the number of directories: %v", puts)
	}
}

// Packing must not add driver Gets on a tree it has nothing to pack in.
func TestPackDetectionCostsNoExtraDriverGets(t *testing.T) {
	m := newManager(t)
	src := t.TempDir()
	for d := 0; d < 12; d++ {
		for f := 0; f < 3; f++ {
			write(t, src, fmt.Sprintf("d%02d/s%d/f%d.txt", d, d%3, f), strings.Repeat("x", 2000))
		}
	}
	plainDrv, packDrv := fakeMount(t, "/fk-plain"), fakeMount(t, "/fk-pack")
	spec := func(dst string, pack bool) syncjob.Spec {
		sp := syncjob.Spec{Mode: syncjob.ModeSync, Src: "local:" + src, Dst: dst}
		if pack {
			sp.Pack = &syncjob.PackOptions{Enabled: true, Threshold: 1024}
		}
		return sp
	}
	for _, drv := range []*fakedriver.Driver{plainDrv, packDrv} {
		for d := 0; d < 12; d++ {
			drv.AddDir(fmt.Sprintf("/d%02d", d))
			drv.AddDir(fmt.Sprintf("/d%02d/s%d", d, d%3))
			for f := 0; f < 3; f++ {
				drv.AddFile(fmt.Sprintf("/d%02d/s%d/f%d.txt", d, d%3, f), strings.Repeat("x", 2000))
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
		return drv.Gets.Load()
	}
	plain := count(plainDrv, "/fk-plain", false)
	packed := count(packDrv, "/fk-pack", true)
	t.Logf("driver Gets: plain=%d pack=%d", plain, packed)
	// The manifest lookups are a constant, not per directory.
	if packed > plain+4 {
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

var _ = io.EOF

// --- migration from version 1 per-directory packs ---------------------------

func TestPackMigratesVersion1DirectoryPacks(t *testing.T) {
	mt := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	for _, mode := range []syncjob.Mode{syncjob.ModeCopy, syncjob.ModeSync} {
		m := newManager(t)
		src, dst, out := tempDir(t), tempDir(t), tempDir(t)
		// src still has a.json and b.json; c.json exists only in the v1 pack.
		write(t, src, "old/dir/a.json", `{"a":1}`)
		write(t, src, "old/dir/b.json", `{"b":2}`)
		write(t, src, "rootfile.txt", "root")
		write(t, src, "other/big.bin", strings.Repeat("B", 3000))
		buildV1(t, dst, "old/dir", map[string]string{"a.json": `{"a":1}`, "b.json": `{"b":2}`, "c.json": `{"c":3}`}, mt)
		buildV1(t, dst, "", map[string]string{"rootfile.txt": "root", "rootgone.txt": "only in v1"}, mt)
		// A v1 directory src doesn't have any more.
		buildV1(t, dst, "vanished", map[string]string{"x.json": `{"x":1}`}, mt)

		s := mustSucceed(t, runJob(t, m, packSpec(mode, src, dst, syncjob.PackOptions{Threshold: 1024})))
		if s.PackStats.V1MigratedDirs != 3 {
			t.Fatalf("%s: v1_migrated_dirs = %d, want 3 (%+v)", mode, s.PackStats.V1MigratedDirs, s.PackStats)
		}
		for _, gone := range []string{"old/dir/.oppack", "vanished/.oppack", ".oppack/index.json", ".oppack/p-000001.zip"} {
			if exists(dst, gone) {
				t.Fatalf("%s: v1 leftover %s", mode, gone)
			}
		}
		if !exists(dst, ".oppack/manifest.json.gz") || !exists(dst, "other/big.bin") {
			t.Fatalf("%s: job manifest or large file missing", mode)
		}
		if loose := looseSmall(t, dst, 1024); len(loose) != 0 {
			t.Fatalf("%s: loose small files %v", mode, loose)
		}

		mf := loadTestManifest(t, dst)
		kept := "old/dir/a.json,old/dir/b.json,old/dir/c.json,rootfile.txt,rootgone.txt,vanished/x.json"
		if mode == syncjob.ModeSync {
			// Sync lets go of what src doesn't have.
			kept = "old/dir/a.json,old/dir/b.json,rootfile.txt"
		}
		if got := strings.Join(fileNames(mf), ","); got != kept {
			t.Fatalf("%s: manifest files = %s\nwant %s", mode, got, kept)
		}
		mustSucceed(t, runJob(t, m, unpackSpec(syncjob.ModeCopy, dst, out)))
		for rel, want := range map[string]string{"old/dir/a.json": `{"a":1}`, "rootfile.txt": "root"} {
			if got, _ := read(t, out, rel); got != want {
				t.Fatalf("%s: %s = %q", mode, rel, got)
			}
		}
		if mode == syncjob.ModeCopy {
			// Members that existed only in v1 survive byte for byte.
			for rel, want := range map[string]string{"old/dir/c.json": `{"c":3}`, "rootgone.txt": "only in v1", "vanished/x.json": `{"x":1}`} {
				if got, _ := read(t, out, rel); got != want {
					t.Fatalf("%s: migrated %s = %q, want %q", mode, rel, got, want)
				}
			}
		}

		// A second run has nothing left to migrate or write.
		s = mustSucceed(t, runJob(t, m, packSpec(mode, src, dst, syncjob.PackOptions{Threshold: 1024})))
		if s.PackStats.V1MigratedDirs != 0 || s.PackStats.PacksWritten != 0 {
			t.Fatalf("%s: second run stats = %+v", mode, s.PackStats)
		}
	}
}

// A run that died before its commit leaves packs no manifest refers to; the
// next committed run removes them, and only them.
func TestPackOrphanPacksAreRemovedAfterACommit(t *testing.T) {
	m := newManager(t)
	src := tempDir(t)
	for i := 0; i < 4; i++ {
		write(t, src, fmt.Sprintf("d/f%d.txt", i), fmt.Sprintf("v1 %d", i))
	}
	drv := fakeMount(t, "/fk-orphan")
	spec := syncjob.Spec{Mode: syncjob.ModeSync, Src: "local:" + src, Dst: "/fk-orphan",
		Pack: &syncjob.PackOptions{Enabled: true, Threshold: 1024, MaxPackBytes: 1}}
	mustSucceed(t, runJob(t, m, spec))
	time.Sleep(200 * time.Millisecond)

	write(t, src, "d/f1.txt", "v2 edited")
	write(t, src, "d/new.txt", "brand new")
	var puts int
	drv.FailPut = func(path string) error {
		if strings.HasPrefix(path, "/.oppack/p-") {
			if puts++; puts == 2 {
				return fmt.Errorf("injected failure on %s", path)
			}
		}
		return nil
	}
	if s := runJob(t, m, spec); s.State != syncjob.StateFailed {
		t.Fatalf("job state = %s, want failed", s.State)
	}
	var orphan string // the first run made p-000001..4, one per file
	for _, n := range []string{"p-000005.zip", "p-000006.zip", "p-000007.zip"} {
		if _, ok := drv.Content("/.oppack/" + n); ok {
			orphan = n
		}
	}
	if orphan == "" {
		t.Fatal("the failed run left no orphan pack")
	}
	// Files that are not packs must survive.
	drv.FailPut = nil
	s := mustSucceed(t, runJob(t, m, spec))
	time.Sleep(200 * time.Millisecond)
	mf := make(map[string]bool)
	raw, _ := drv.Content("/.oppack/manifest.json.gz")
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var tm testManifest
	if err := json.NewDecoder(zr).Decode(&tm); err != nil {
		t.Fatal(err)
	}
	for name := range tm.Packs {
		mf[name] = true
		if _, ok := drv.Content("/.oppack/" + name); !ok {
			t.Fatalf("referenced pack %s was removed", name)
		}
	}
	if s.PackStats.OrphanPacksRemoved < 1 {
		t.Fatalf("stats = %+v, want orphans removed", s.PackStats)
	}
	for i := 1; i <= 10; i++ {
		n := fmt.Sprintf("p-%06d.zip", i)
		_, there := drv.Content("/.oppack/" + n)
		if there != mf[n] {
			t.Fatalf("%s: at dst = %v, referenced = %v", n, there, mf[n])
		}
	}
	if _, ok := drv.Content("/.oppack/manifest.prev.json.gz"); !ok {
		t.Fatal("manifest.prev was removed")
	}
}

func TestPackOrphanCleanupSparesOtherFilesAndDryRunOnlyCounts(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	write(t, src, "a/x.txt", "x")
	opts := syncjob.PackOptions{Threshold: 1024}
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, opts)))

	write(t, dst, ".oppack/p-000009.zip", "orphan")
	write(t, dst, ".oppack/p-9.zip", "not a job pack name")
	write(t, dst, ".oppack/notes.txt", "mine")

	sp := packSpec(syncjob.ModeCopy, src, dst, opts)
	sp.DryRun = true
	s := mustSucceed(t, runJob(t, m, sp))
	if s.PackStats.OrphanPacksRemoved != 1 || !exists(dst, ".oppack/p-000009.zip") {
		t.Fatalf("dry run: stats = %+v, orphan present = %v", s.PackStats, exists(dst, ".oppack/p-000009.zip"))
	}

	s = mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, opts)))
	if s.PackStats.OrphanPacksRemoved != 1 || exists(dst, ".oppack/p-000009.zip") {
		t.Fatalf("stats = %+v, orphan present = %v", s.PackStats, exists(dst, ".oppack/p-000009.zip"))
	}
	for _, keep := range []string{".oppack/p-9.zip", ".oppack/notes.txt", ".oppack/manifest.json.gz", ".oppack/p-000001.zip"} {
		if !exists(dst, keep) {
			t.Fatalf("%s was removed", keep)
		}
	}
}

// The root's v1 pack shares .oppack with the job packs and has the same
// names; until it is migrated it is not an orphan.
func TestPackOrphanCleanupSparesRootV1PackBeforeMigration(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	write(t, src, "rootfile.txt", "root")
	mt := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	buildV1(t, dst, "", map[string]string{"rootfile.txt": "root", "rootgone.txt": "only in v1"}, mt)
	// A manifest has to exist for the cleanup to judge against.
	opts := syncjob.PackOptions{Threshold: 1024}
	sp := packSpec(syncjob.ModeCopy, src, dst, opts)
	sp.DryRun = true
	s := mustSucceed(t, runJob(t, m, sp))
	if s.PackStats.OrphanPacksRemoved != 0 || !exists(dst, ".oppack/p-000001.zip") || !exists(dst, ".oppack/index.json") {
		t.Fatalf("dry run: stats = %+v", s.PackStats)
	}
	s = mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, opts)))
	if s.PackStats.OrphanPacksRemoved != 0 {
		t.Fatalf("v1 pack counted as orphan: %+v", s.PackStats)
	}
	out := tempDir(t)
	mustSucceed(t, runJob(t, m, unpackSpec(syncjob.ModeCopy, dst, out)))
	if got, _ := read(t, out, "rootgone.txt"); got != "only in v1" {
		t.Fatalf("migrated member lost: %q", got)
	}
}
