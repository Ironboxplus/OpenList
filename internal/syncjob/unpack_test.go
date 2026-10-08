package syncjob_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/syncjob"
	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs/config/configmap"
)

func unpackSpec(mode syncjob.Mode, src, dst string) syncjob.Spec {
	return syncjob.Spec{Mode: mode, Src: "local:" + src, Dst: "local:" + dst, Unpack: true}
}

// tree maps every file under root to its mtime and content.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		out[filepath.ToSlash(rel)] = fmt.Sprintf("%d|%x", info.ModTime().UnixNano(), b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, want, got map[string]string) {
	t.Helper()
	for k, v := range want {
		g, ok := got[k]
		switch {
		case !ok:
			t.Errorf("missing %q", k)
		case g != v:
			t.Errorf("%q differs (mtime|content): want %.60s got %.60s", k, v, g)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected %q", k)
		}
	}
}

func buildRoundTripTree(t *testing.T, src string) {
	t.Helper()
	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	i := 0
	put := func(rel, content string) {
		write(t, src, rel, content)
		setMTime(t, src, rel, base.Add(time.Duration(i)*time.Minute))
		i++
	}
	for d := 0; d < 3; d++ {
		for f := 0; f < 5; f++ {
			put(fmt.Sprintf("a/b/c%d/n%d.json", d, f), strings.Repeat(fmt.Sprintf(`{"k":%d}`, f), 30))
		}
		put(fmt.Sprintf("a/b/c%d/model.pt", d), strings.Repeat("W", 300))
		put(fmt.Sprintf("a/b/c%d/huge.bin", d), strings.Repeat("H", 9000))
	}
	put("a/b/c0/empty.txt", "")
	put("中文 目录/文件 一.txt", "你好")
	put("中文 目录/文件 二.txt", "世界")
	put("中文 目录/名字 with space.md", "# title")
	put("中文 目录/四.txt", "four")
	put("中文 目录/五.txt", "five")
	put("few/one.txt", "1")
	put("few/two.txt", "2")
	put("top.txt", "top-level loose")
}

func TestPackUnpackRoundTrip(t *testing.T) {
	m := newManager(t)
	src, packed, out := tempDir(t), tempDir(t), tempDir(t)
	buildRoundTripTree(t, src)

	s := mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 1024})))
	if s.PackStats.PacksWritten == 0 {
		t.Fatalf("nothing packed: %+v", s.PackStats)
	}
	if loose := looseSmall(t, packed, 1024); len(loose) != 0 {
		t.Fatalf("small files uploaded on their own: %v", loose)
	}
	if !exists(packed, "a/b/c1/huge.bin") {
		t.Fatalf("large file missing from the packed tree")
	}

	s = mustSucceed(t, runJob(t, m, unpackSpec(syncjob.ModeCopy, packed, out)))
	if s.Stage != "transferring" || s.PackStats.UnpackedFiles != 3*6+1+5+2+1 {
		t.Fatalf("unpack stats = %+v (stage %s)", s.PackStats, s.Stage)
	}
	sameTree(t, tree(t, src), tree(t, out))

	// Second run extracts nothing.
	s = mustSucceed(t, runJob(t, m, unpackSpec(syncjob.ModeCopy, packed, out)))
	if s.PackStats.UnpackedFiles != 0 {
		t.Fatalf("unchanged unpack extracted %d files", s.PackStats.UnpackedFiles)
	}
	// A missing or damaged file is restored, and only that file.
	if err := os.Remove(filepath.Join(out, "a", "b", "c2", "n1.json")); err != nil {
		t.Fatal(err)
	}
	write(t, out, "中文 目录/四.txt", "tampered!")
	s = mustSucceed(t, runJob(t, m, unpackSpec(syncjob.ModeCopy, packed, out)))
	if s.PackStats.UnpackedFiles != 2 {
		t.Fatalf("repair extracted %d files, want 2", s.PackStats.UnpackedFiles)
	}
	sameTree(t, tree(t, src), tree(t, out))
}

func TestUnpackSyncDeletesExtrasButKeepsPackedFiles(t *testing.T) {
	m := newManager(t)
	src, packed, out := tempDir(t), tempDir(t), tempDir(t)
	for i := 0; i < 4; i++ {
		write(t, src, fmt.Sprintf("d/f%d.txt", i), "small")
	}
	write(t, src, "d/big.bin", strings.Repeat("B", 3000))
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 1024})))

	write(t, out, "d/extra.txt", "not in the backup")
	write(t, out, "gone/x.txt", "nor this")
	for i := 0; i < 2; i++ {
		mustSucceed(t, runJob(t, m, unpackSpec(syncjob.ModeSync, packed, out)))
		if exists(out, "d/extra.txt") || exists(out, "gone/x.txt") {
			t.Fatalf("run %d: sync left extras", i)
		}
		if !exists(out, "d/f0.txt") || !exists(out, "d/big.bin") {
			t.Fatalf("run %d: sync deleted unpacked files", i)
		}
		if exists(out, "d/.oppack") {
			t.Fatalf("run %d: .oppack copied to the unpack target", i)
		}
	}
}

func TestUnpackByRangeReadsOnlyWhatItNeeds(t *testing.T) {
	defer syncjob.SetRangeThresholds(1, 2, 4096)()
	m := newManager(t)
	src, packed, out := tempDir(t), tempDir(t), tempDir(t)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 60; i++ {
		b := make([]byte, 2000)
		rng.Read(b)
		write(t, src, fmt.Sprintf("d/f%02d.dat", i), string(b))
	}
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 4096, StoreSuffixes: []string{".dat"}})))
	mustSucceed(t, runJob(t, m, unpackSpec(syncjob.ModeCopy, packed, out)))

	// Two files missing at dst: 4KB wanted out of a 120KB pack.
	_ = os.Remove(filepath.Join(out, "d", "f07.dat"))
	_ = os.Remove(filepath.Join(out, "d", "f41.dat"))
	s := mustSucceed(t, runJob(t, m, unpackSpec(syncjob.ModeCopy, packed, out)))
	if s.PackStats.PacksRanged != 1 || s.PackStats.PacksFetched != 0 || s.PackStats.UnpackedFiles != 2 {
		t.Fatalf("stats = %+v, want one ranged pack and 2 files", s.PackStats)
	}
	sameTree(t, tree(t, src), tree(t, out))

	// The reader itself fetches far less than the pack.
	ctx := context.Background()
	lf, err := local.NewFs(ctx, "t", filepath.Join(packed, ".oppack"), configmap.Simple{})
	if err != nil {
		t.Fatal(err)
	}
	o, err := lf.NewObject(ctx, "p-000001.zip")
	if err != nil {
		t.Fatal(err)
	}
	ra, counters := syncjob.NewRangeReaderAt(ctx, o, 4096)
	zr, err := zip.NewReader(ra, o.Size())
	if err != nil {
		t.Fatal(err)
	}
	rc, err := zr.File[30].Open()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rc)
	_ = rc.Close()
	if buf.Len() != 2000 {
		t.Fatalf("member read %d bytes", buf.Len())
	}
	if _, fetched := counters(); fetched > o.Size()/2 {
		t.Fatalf("range reader fetched %d of %d bytes to read one member", fetched, o.Size())
	}
}

func TestUnpackRefusesCorruptPack(t *testing.T) {
	m := newManager(t)
	src, packed, out := tempDir(t), tempDir(t), tempDir(t)
	for i := 0; i < 4; i++ {
		write(t, src, fmt.Sprintf("bad/f%d.txt", i), "damaged soon")
		write(t, src, fmt.Sprintf("good/f%d.txt", i), "healthy")
	}
	write(t, src, "big.bin", strings.Repeat("B", 3000))
	// A pack per file, so one bad pack costs one file.
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 1024, MaxPackBytes: 1})))
	f, err := os.OpenFile(filepath.Join(packed, ".oppack", "p-000001.zip"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("junk"))
	_ = f.Close()

	s := runJob(t, m, unpackSpec(syncjob.ModeCopy, packed, out))
	if s.State != syncjob.StateFailed || !strings.Contains(s.Error, "corrupt") {
		t.Fatalf("state %s error %q, want failed with a corrupt-pack error", s.State, s.Error)
	}
	if exists(out, "bad/f0.txt") {
		t.Fatalf("content of a corrupt pack was written")
	}
	if !exists(out, "bad/f1.txt") || !exists(out, "good/f0.txt") || !exists(out, "big.bin") {
		t.Fatalf("one bad pack stopped the rest")
	}
}

func TestUnpackOnlyForCopyAndSync(t *testing.T) {
	m := newManager(t)
	mountLocal(t, "/job-unpack-bad")
	for _, mode := range []syncjob.Mode{syncjob.ModeBisync, syncjob.ModeCheck} {
		if _, err := m.Start(syncjob.Spec{Mode: mode, Src: "/job-unpack-bad", Dst: "/job-unpack-bad", Unpack: true}); err == nil {
			t.Errorf("unpack accepted with %s", mode)
		}
	}
	both := syncjob.Spec{Mode: syncjob.ModeCopy, Src: "/job-unpack-bad", Dst: "/job-unpack-bad", Unpack: true, Pack: &syncjob.PackOptions{Enabled: true}}
	if _, err := m.Start(both); err == nil {
		t.Errorf("pack and unpack together accepted")
	}
}

// --- legacy PACKED.tsv -----------------------------------------------------

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: mt, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func writeBytes(t *testing.T, root, rel string, b []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUnpackLegacyPackedTSV(t *testing.T) {
	m := newManager(t)
	tgz := makeTarGz(t, map[string]string{"proj/a.txt": "alpha", "proj/sub/b.txt": "bravo"})
	zp := makeZip(t, map[string]string{"proj2/c.txt": "charlie"})

	for _, c := range []struct {
		index, archive, rows string
		archiveBytes         []byte
		want                 map[string]string
	}{
		{"PACKED.tsv", "proj.tar.gz", "dir\tarchive\tfiles\tbytes\tsha256\nproj\tproj.tar.gz\t2\t10\t" + sha256hex(tgz) + "\n", tgz,
			map[string]string{"proj/a.txt": "alpha", "proj/sub/b.txt": "bravo"}},
		{"PACKED2.tsv", "x/y/proj2.zip", "x/y/proj2\tx/y/proj2.zip\t1\t7\t" + sha256hex(zp) + "\n", zp,
			map[string]string{"x/y/proj2/c.txt": "charlie"}},
	} {
		src, out := tempDir(t), tempDir(t)
		write(t, src, c.index, c.rows)
		writeBytes(t, src, c.archive, c.archiveBytes)
		write(t, src, "plain.txt", "ordinary")

		for _, mode := range []syncjob.Mode{syncjob.ModeCopy, syncjob.ModeSync} {
			mustSucceed(t, runJob(t, m, unpackSpec(mode, src, out)))
			for rel, want := range c.want {
				if got, _ := read(t, out, rel); got != want {
					t.Fatalf("%s %s: %s = %q, want %q", c.index, mode, rel, got, want)
				}
			}
			if got, _ := read(t, out, "plain.txt"); got != "ordinary" {
				t.Fatalf("plain file not copied")
			}
			for _, left := range []string{c.index, c.archive} {
				if exists(out, left) {
					t.Fatalf("%s %s: %s copied as an ordinary file", c.index, mode, left)
				}
			}
		}
	}
}

func TestUnpackLegacyRefusesWrongSha256(t *testing.T) {
	m := newManager(t)
	tgz := makeTarGz(t, map[string]string{"proj/a.txt": "alpha"})
	src, out := tempDir(t), tempDir(t)
	write(t, src, "PACKED.tsv", "proj\tproj.tar.gz\t1\t5\t"+strings.Repeat("0", 64)+"\n")
	writeBytes(t, src, "proj.tar.gz", tgz)
	s := runJob(t, m, unpackSpec(syncjob.ModeCopy, src, out))
	if s.State != syncjob.StateFailed || !strings.Contains(s.Error, "sha256") {
		t.Fatalf("state %s error %q, want failed on sha256", s.State, s.Error)
	}
	if exists(out, "proj/a.txt") {
		t.Fatalf("archive with a wrong sha256 was extracted")
	}
}

func TestUnpackLegacyRefusesEscapingEntries(t *testing.T) {
	m := newManager(t)
	tgz := makeTarGz(t, map[string]string{"proj/ok.txt": "fine", "proj/../../escape.txt": "evil"})
	base := tempDir(t)
	src, out := filepath.Join(base, "src"), filepath.Join(base, "out")
	_ = os.MkdirAll(src, 0o755)
	_ = os.MkdirAll(out, 0o755)
	write(t, src, "PACKED.tsv", "proj\tproj.tar.gz\t2\t8\t"+sha256hex(tgz)+"\n")
	writeBytes(t, src, "proj.tar.gz", tgz)
	s := runJob(t, m, unpackSpec(syncjob.ModeCopy, src, out))
	if s.State != syncjob.StateFailed {
		t.Fatalf("state %s, want failed", s.State)
	}
	if exists(base, "escape.txt") || exists(out, "escape.txt") {
		t.Fatalf("an archive entry escaped the target directory")
	}
}

func TestUnpackPrefersPackMemberOverLooseSameName(t *testing.T) {
	m := newManager(t)
	src, packed, out := tempDir(t), tempDir(t), tempDir(t)
	for i := 0; i < 4; i++ {
		write(t, src, fmt.Sprintf("d/f%d.txt", i), "from the pack")
	}
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 1024})))
	write(t, packed, "d/f1.txt", "loose impostor")

	for _, mode := range []syncjob.Mode{syncjob.ModeCopy, syncjob.ModeSync} {
		s := mustSucceed(t, runJob(t, m, unpackSpec(mode, packed, out)))
		if s.PackStats.Conflicts != 1 {
			t.Fatalf("%s: conflicts = %d, want 1", mode, s.PackStats.Conflicts)
		}
		if got, _ := read(t, out, "d/f1.txt"); got != "from the pack" {
			t.Fatalf("%s: d/f1.txt = %q, want the pack member", mode, got)
		}
	}
}

// The point of full-path members: unzip at the root restores the tree.
func TestPackMembersRestoreTheTreeWithPlainUnzip(t *testing.T) {
	src, packed, out := tempDir(t), tempDir(t), tempDir(t)
	buildRoundTripTree(t, src)
	mustSucceed(t, runJob(t, newManager(t), packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 1024, MaxPackBytes: 600})))
	if n := len(packNames(t, packed)); n < 3 {
		t.Fatalf("only %d packs; the test wants several", n)
	}
	for _, name := range packNames(t, packed) {
		zr, err := zip.OpenReader(filepath.Join(packed, ".oppack", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			writeBytes(t, out, f.Name, b)
		}
		_ = zr.Close()
	}
	for _, rel := range looseSmall(t, src, 1024) {
		a, _ := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s not restored by unzip (err %v)", rel, err)
		}
	}
}

// If the commit died between saving prev and writing the new manifest, the
// next reader falls back to prev.
func TestUnpackFallsBackToPrevManifest(t *testing.T) {
	src, packed, out := tempDir(t), tempDir(t), tempDir(t)
	for i := 0; i < 3; i++ {
		write(t, src, fmt.Sprintf("d/f%d.txt", i), "content")
	}
	mustSucceed(t, runJob(t, newManager(t), packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 1024})))
	if err := os.Rename(filepath.Join(packed, ".oppack", "manifest.json.gz"), filepath.Join(packed, ".oppack", "manifest.prev.json.gz")); err != nil {
		t.Fatal(err)
	}
	mustSucceed(t, runJob(t, newManager(t), unpackSpec(syncjob.ModeCopy, packed, out)))
	if got, _ := read(t, out, "d/f2.txt"); got != "content" {
		t.Fatalf("d/f2.txt = %q", got)
	}
}

// buildV1 writes a version-1 per-directory pack the way earlier releases did.
func buildV1(t *testing.T, root, dir string, files map[string]string, mtime time.Time) {
	t.Helper()
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	idx := map[string]any{}
	for name, body := range files {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mtime})
		_, _ = w.Write([]byte(body))
		sum := sha1.Sum([]byte(body))
		idx[name] = map[string]any{"size": len(body), "mtime": mtime.UTC().Format(time.RFC3339Nano), "sha1": hex.EncodeToString(sum[:]), "pack": "p-000001.zip"}
	}
	_ = zw.Close()
	pk := sha1.Sum(zb.Bytes())
	writeBytes(t, root, path.Join(dir, ".oppack", "p-000001.zip"), zb.Bytes())
	manifest, _ := json.Marshal(map[string]any{
		"version": 1, "threshold": 1024, "files": idx,
		"packs": map[string]any{"p-000001.zip": map[string]any{"bytes": zb.Len(), "sha1": hex.EncodeToString(pk[:]), "live": len(files), "dead": 0}},
	})
	writeBytes(t, root, path.Join(dir, ".oppack", "index.json"), manifest)
}

func TestUnpackStillReadsVersion1DirectoryPacks(t *testing.T) {
	packed, out := tempDir(t), tempDir(t)
	mt := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	buildV1(t, packed, "old/dir", map[string]string{"a.json": `{"a":1}`, "b 二.json": `{"b":2}`}, mt)
	buildV1(t, packed, "", map[string]string{"rootfile.txt": "root"}, mt)
	write(t, packed, "old/dir/big.bin", strings.Repeat("B", 3000))
	s := mustSucceed(t, runJob(t, newManager(t), unpackSpec(syncjob.ModeSync, packed, out)))
	if s.PackStats.UnpackedFiles != 3 {
		t.Fatalf("stats = %+v", s.PackStats)
	}
	for rel, want := range map[string]string{"old/dir/a.json": `{"a":1}`, "old/dir/b 二.json": `{"b":2}`, "rootfile.txt": "root"} {
		if got, _ := read(t, out, rel); got != want {
			t.Fatalf("%s = %q, want %q", rel, got, want)
		}
	}
	if exists(out, "old/dir/.oppack") || exists(out, ".oppack") || !exists(out, "old/dir/big.bin") {
		t.Fatalf("unpack target layout wrong")
	}
}
