package identify

import (
	"fmt"
	"testing"
)

// 115's offline downloads: every episode is a directory named like the file it holds.
func wrapped(names ...string) (entries []Entry, tree map[string][]Entry) {
	tree = map[string][]Entry{}
	for _, n := range names {
		entries = append(entries, Entry{Name: n, IsDir: true})
		tree[n] = []Entry{{Name: n, Size: gb}}
	}
	return
}

func peekOf(tree map[string][]Entry) Peek {
	return func(name string) ([]Entry, error) {
		if es, ok := tree[name]; ok {
			return es, nil
		}
		return nil, fmt.Errorf("%s: not listed", name)
	}
}

func TestUnwrapEntries(t *testing.T) {
	es, tree := wrapped("[ANi] 婚姻劇毒 - 01 [1080P].mp4", "[ANi] 婚姻劇毒 - 02 [1080P].mp4")
	es = append(es, Entry{Name: "SPs", IsDir: true})
	got := UnwrapEntries(es, peekOf(tree))
	if len(got) != 3 || got[0].IsDir || got[0].Via != "[ANi] 婚姻劇毒 - 01 [1080P].mp4" || got[0].Rel() != got[0].Via+"/"+got[0].Name || got[2].Name != "SPs" {
		t.Fatalf("got %+v", got)
	}
	// without a peek, or when it fails, the directory stays
	if g := UnwrapEntries(es, nil); g[0].Via != "" || !g[0].IsDir {
		t.Fatalf("no peek: %+v", g[0])
	}
	if g := UnwrapEntries(es[:1], peekOf(nil)); g[0].Via != "" || !g[0].IsDir {
		t.Fatalf("failed peek: %+v", g[0])
	}
}

func TestShowOfWrappedEpisodesIsAShowNotACollectionOfFilms(t *testing.T) {
	es, tree := wrapped(
		"[ANi] 婚姻劇毒 - 01 [1080P][Baha].mp4",
		"[ANi] 婚姻劇毒 - 02 [1080P][Baha].mp4",
		"[ANi] 婚姻劇毒 - 03 [1080P][Baha].mp4",
	)
	r := ClassifyWith("剧毒", true, es, peekOf(tree))
	if wantedKind(r) != "tv" {
		t.Fatalf("kind %q (%s), want tv", r.Kind, r.Why)
	}
}

func TestWrappedEpisodesOfTwoShowsAreNotOneShow(t *testing.T) {
	es, tree := wrapped(
		"[ANi] Re：從零開始的異世界生活 第四季 - 10 [1080P].mp4",
		"[ANi] Re：從零開始的異世界生活 第四季 - 11 [1080P].mp4",
		"[ANi] 杖與劍的魔劍譚 Season 2 - 21 [1080P].mp4",
		"[ANi] 杖與劍的魔劍譚 Season 2 - 22 [1080P].mp4",
	)
	if r := ClassifyWith("re0", true, es, peekOf(tree)); r.Kind != "collection" {
		t.Fatalf("kind %q (%s), want collection", r.Kind, r.Why)
	}
}

func TestADirectoryNamedLikeItsVideoIsThatFile(t *testing.T) {
	name := "JoJos.Bizarre.Adventure.S06E01.STEEL.BALL.RUN.1080p.NF.WEB-DL.MULTi.AAC2.0.H.264.MSubs-ToonsHub.mkv"
	r := ClassifyWith(name, true, []Entry{{Name: name, Size: 2 * gb}}, nil)
	if wantedKind(r) != "tv" || r.Season != 6 || hasSuffixFold(r.Title, "mkv") {
		t.Fatalf("kind %q season %d title %q (%s)", r.Kind, r.Season, r.Title, r.Why)
	}
}

func hasSuffixFold(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

func TestEpisodeFilesOfWrappedEpisodesPointInsideTheWrapper(t *testing.T) {
	es, tree := wrapped("[ANi] 婚姻劇毒 - 01 [1080P].mp4", "[ANi] 婚姻劇毒 - 02 [1080P].mp4")
	tree[""] = es
	got, err := EpisodeFiles(func(rel string) ([]Entry, error) { return tree[rel], nil })
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v (%v)", got, err)
	}
	if want := "[ANi] 婚姻劇毒 - 01 [1080P].mp4/[ANi] 婚姻劇毒 - 01 [1080P].mp4"; got[0].Rel != want || got[0].Episode != 1 {
		t.Fatalf("got %+v, want rel %q", got[0], want)
	}
}

func TestEpisodeFileTitlesKeepQuotationsAndDropTheEpisodeNumber(t *testing.T) {
	for name, want := range map[string]string{
		"[ANi] 想結束這場「我愛你」的遊戲 - 03 [1080P][Baha][WEB-DL][AAC AVC][CHT].mp4":     "想結束這場「我愛你」的遊戲",
		"[ANi] 婚姻劇毒 - 03 [1080P][Baha][WEB-DL][AAC AVC][CHT].mp4":              "婚姻劇毒",
		"[ANi] Re：從零開始的異世界生活 第四季 - 19 [1080P][Baha][WEB-DL][AAC AVC][CHT].mp4": "Re：從零開始的異世界生活",
	} {
		if got := ParseName(name, true).Title; got != want {
			t.Errorf("%s: title %q, want %q", name, got, want)
		}
	}
}

func TestShowDirectoryOffersTheTitleOfItsEpisodeFilesToTheSearch(t *testing.T) {
	es, tree := wrapped(
		"[ANi] 想結束這場「我愛你」的遊戲 - 01 [1080P].mp4",
		"[ANi] 想結束這場「我愛你」的遊戲 - 02 [1080P].mp4",
	)
	r := ClassifyWith("想结束", true, es, peekOf(tree))
	found := false
	for _, c := range r.Content {
		found = found || c.Name == "想結束這場「我愛你」的遊戲"
	}
	if !found {
		t.Fatalf("content %+v", r.Content)
	}
}
