package identify

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// The recorded listing of the user's video library (/115/arc/MOV/{Live,Anime},
// names and sizes only, three levels below each root, recorded 2026-11-03):
// the offline replay every identification rule must keep passing.
type libNode struct {
	E [][]any             `json:"e"` // [name, size] or [name, 0, 1] for a directory
	C map[string]*libNode `json:"c"`
}

func (n *libNode) entries() []Entry {
	out := make([]Entry, 0, len(n.E))
	for _, e := range n.E {
		ent := Entry{Name: e[0].(string), Size: int64(e[1].(float64))}
		ent.IsDir = len(e) > 2
		out = append(out, ent)
	}
	return out
}

func loadLibrary(t testing.TB) map[string]*libNode {
	return loadLibraryFile(t, "testdata/mov_library.json.gz")
}

func loadLibraryFile(t testing.TB, file string) map[string]*libNode {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var lib map[string]*libNode
	if err := json.NewDecoder(zr).Decode(&lib); err != nil {
		t.Fatal(err)
	}
	return lib
}

// replay classifies every work of the library (and the children of the
// collections, as the scanner does, down to depth 3) the way the scanner does:
// the name and the one-level listing. Keys are "Live/<work>[/<child>...]".
func replay(t testing.TB) (keys []string, got map[string]Result) {
	return replayLibrary(t, loadLibrary(t), "Live", "Anime")
}

func replayLibrary(t testing.TB, lib map[string]*libNode, roots ...string) (keys []string, got map[string]Result) {
	got = map[string]Result{}
	var walk func(key, name string, isDir bool, n *libNode, depth int)
	walk = func(key, name string, isDir bool, n *libNode, depth int) {
		var entries []Entry
		if isDir && n != nil {
			entries = n.entries()
		}
		var peek Peek
		if n != nil {
			peek = func(child string) ([]Entry, error) {
				c := n.C[child]
				if c == nil {
					return nil, fmt.Errorf("%s/%s was not recorded", key, child)
				}
				return c.entries(), nil
			}
		}
		r := ClassifyWith(name, isDir, entries, peek)
		keys = append(keys, key)
		got[key] = r
		if r.Kind == "collection" && n != nil && depth < 3 {
			for _, e := range n.entries() {
				if !e.IsDir || IsExtraDir(e.Name) {
					continue
				}
				walk(key+"/"+e.Name, e.Name, true, n.C[e.Name], depth+1)
			}
		}
	}
	for _, root := range roots {
		top := lib[root]
		for _, e := range top.entries() {
			walk(root+"/"+e.Name, e.Name, e.IsDir, top.C[e.Name], 1)
		}
	}
	return keys, got
}

// TestDumpLibrary prints the verdict for every work: LIBRARY_DUMP=1 go test -run DumpLibrary -v
func TestDumpLibrary(t *testing.T) {
	if os.Getenv("LIBRARY_DUMP") == "" {
		t.Skip("set LIBRARY_DUMP=1 to print the verdicts")
	}
	keys, got := replay(t)
	var sb strings.Builder
	for _, k := range keys {
		r := got[k]
		fmt.Fprintf(&sb, "%s\t%s\t%.2f\t%q y%d s%d\t%s\n", k, r.Kind, r.Confidence, r.Title, r.Year, r.Season, r.Why)
	}
	_ = sort.Strings
	if err := os.WriteFile(os.Getenv("LIBRARY_DUMP"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantedKind(r Result) string {
	if r.Kind == "anime" {
		if r.Theatrical {
			return "movie"
		}
		return "tv"
	}
	return r.Kind
}

// TestLibraryReplay is the acceptance table: every top-level work of the
// recorded library (141) and the children of collections that were checked by
// hand, with the kind the rules must give. Rows marked "provider" can only be
// decided by the TMDB tv-identity step at scrape time and rows marked
// "doubtful" are an unconfirmed reading: both are reported, not asserted.
func TestLibraryReplay(t *testing.T) {
	_, got := replay(t)
	raw, err := os.ReadFile("testdata/mov_expected.tsv")
	if err != nil {
		t.Fatal(err)
	}
	top, topOK := 0, 0
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		key, want, status := f[0], f[1], f[2]
		r, ok := got[key]
		if !ok {
			t.Errorf("%s: not in the replay", key)
			continue
		}
		isTop := strings.Count(key, "/") == 1
		if isTop {
			top++
		}
		have := wantedKind(r)
		if have == want {
			if isTop {
				topOK++
			}
			continue
		}
		if status == "rule" {
			t.Errorf("%s: kind %q (%s), want %q", key, have, r.Why, want)
		} else {
			t.Logf("%s [%s]: kind %q (%s), reading %q", key, status, have, r.Why, want)
		}
	}
	if top != 141 {
		t.Errorf("table has %d top-level works, the library has 141", top)
	}
	t.Logf("top-level works with the wanted kind: %d of %d", topOK, top)
}

// TestYunxiazaiReplay is the same table for the second library (/115/云下载,
// recorded 2026-11-03, round 4 of the findings). A fourth column, when present,
// is the title the rules must give.
func TestYunxiazaiReplay(t *testing.T) {
	keys, got := replayLibrary(t, loadLibraryFile(t, "testdata/yxz_library.json.gz"), "云下载")
	raw, err := os.ReadFile("testdata/yxz_expected.tsv")
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, k := range keys {
		r := got[k]
		fmt.Fprintf(&sb, "%s\t%s\t%.2f\t%q y%d s%d\t%s\n", k, r.Kind, r.Confidence, r.Title, r.Year, r.Season, r.Why)
	}
	if os.Getenv("LIBRARY_DUMP") != "" {
		_ = os.WriteFile(os.Getenv("LIBRARY_DUMP")+".yxz", []byte(sb.String()), 0o644)
	}
	top, topOK := 0, 0
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		key, want, status := f[0], f[1], f[2]
		r, ok := got[key]
		if !ok {
			t.Errorf("%s: not in the replay", key)
			continue
		}
		if strings.Count(key, "/") == 1 {
			top++
		}
		have := wantedKind(r)
		okRow := have == want && (len(f) < 4 || f[3] == "" || f[3] == r.Title)
		if okRow {
			if strings.Count(key, "/") == 1 {
				topOK++
			}
			continue
		}
		msg := fmt.Sprintf("%s [%s]: kind %q title %q (%s), want %q %q", key, status, have, r.Title, r.Why, want, func() string {
			if len(f) > 3 {
				return f[3]
			}
			return ""
		}())
		if status == "rule" {
			t.Error(msg)
		} else {
			t.Log(msg)
		}
	}
	t.Logf("top-level works as wanted: %d of %d", topOK, top)
	if os.Getenv("YXZ_PRINT") != "" || t.Failed() {
		fmt.Print(sb.String())
	}
}
