package apply

import (
	"context"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
)

// kindSearch answers per endpoint kind and counts the requests.
type kindSearch struct {
	hits  map[string][]scraper.Candidate // "tv|title"
	calls []string
}

func (k *kindSearch) Identify(_ context.Context, kind string, _ bool, q scraper.IdentifyQuery) ([]scraper.Candidate, error) {
	for _, t := range q.Terms {
		k.calls = append(k.calls, kind+"|"+t.Name)
		if cs, ok := k.hits[kind+"|"+t.Name]; ok {
			return cs, nil
		}
	}
	return nil, nil
}

func TestCollectionThatIsOneShowOnTMDBBecomesTheShow(t *testing.T) {
	s := &kindSearch{hits: map[string][]scraper.Candidate{
		"tv|星蝶公主":  {{Provider: "tmdb", ID: "tv/1", Title: "星蝶公主", Score: 0.9}},
		"tv|教父":    {{Provider: "tmdb", ID: "tv/2", Title: "教父", Score: 0.7}},
		"movie|教父": {{Provider: "tmdb", ID: "movie/238", Title: "教父", Score: 0.95}},
	}}
	o := &Orchestrator{Search: s}
	got, ok := o.showBehindCollection(context.Background(), identify.Result{Kind: "collection", FromContent: true, Title: "星蝶公主", Parsed: scraper.ParseName("星蝶公主.mkv")})
	if !ok || got.ID != "tv/1" {
		t.Fatalf("星蝶公主: %v %v", got, ok)
	}
	// a film series: the film scores as well as the show, so it stays a collection
	if _, ok := o.showBehindCollection(context.Background(), identify.Result{Kind: "collection", FromContent: true, Title: "教父", Parsed: scraper.ParseName("教父.mkv")}); ok {
		t.Fatal("教父 merged into a show")
	}
	// a collection decided by its name alone is not searched at all
	before := len(s.calls)
	if _, ok := o.showBehindCollection(context.Background(), identify.Result{Kind: "collection", Title: "昆汀", Parsed: scraper.ParseName("昆汀.mkv")}); ok || len(s.calls) != before {
		t.Fatal("a name-only collection was searched")
	}
}
