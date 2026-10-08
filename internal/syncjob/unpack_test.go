package syncjob_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
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

	s := mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 1024, MinFiles: 3})))
	if s.PackStats.Packs == 0 {
		t.Fatalf("nothing packed: %+v", s.PackStats)
	}
	if exists(packed, "a/b/c1/n2.json") || !exists(packed, "a/b/c1/huge.bin") || !exists(packed, "few/one.txt") {
		t.Fatalf("packed layout wrong")
	}

	s = mustSucceed(t, runJob(t, m, unpackSpec(syncjob.ModeCopy, packed, out)))
	if s.Stage != "transferring" || s.PackStats.UnpackedFiles != 3*6+1+5 {
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
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 1024, MinFiles: 3})))

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
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 4096, MinFiles: 3, StoreSuffixes: []string{".dat"}})))
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
	lf, err := local.NewFs(ctx, "t", filepath.Join(packed, "d", ".oppack"), configmap.Simple{})
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
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 1024, MinFiles: 3})))
	f, err := os.OpenFile(filepath.Join(packed, "bad", ".oppack", "p-000001.zip"), os.O_APPEND|os.O_WRONLY, 0)
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
	if !exists(out, "good/f0.txt") {
		t.Fatalf("one bad pack stopped the healthy directory")
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
