package scraper

import (
	"context"
	"net/http"
	"testing"
)

// TMDB answers a detail request in the original language when it has no
// translation: tv/86831 in zh-CN comes back as "Love, Death & Robots" while its
// alternative titles for CN hold "爱，死亡和机器人" (recorded 2026-11-03).
func TestFillUsesTheRegionTitleWhenTMDBHasNoTranslation(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/tv/86831":
			writeJSON(w, []byte(`{"id":86831,"name":"Love, Death & Robots","original_name":"Love, Death & Robots","original_language":"en",
			 "first_air_date":"2019-03-15","alternative_titles":{"results":[
			 {"iso_3166_1":"TW","title":"愛死亡與機器人"},{"iso_3166_1":"CN","title":"爱，死亡和机器人"},{"iso_3166_1":"CN","title":"爱死亡与机器人"}]}}`))
		case "/3/tv/1":
			// already translated: the localized name stands
			writeJSON(w, []byte(`{"id":1,"name":"权力的游戏","original_name":"Game of Thrones","original_language":"en",
			 "alternative_titles":{"results":[{"iso_3166_1":"CN","title":"冰与火之歌"}]}}`))
		case "/3/movie/2":
			// a film without a translation: alternative titles hold the Chinese name
			writeJSON(w, []byte(`{"id":2,"title":"Inception","original_title":"Inception","original_language":"en","release_date":"2010-07-15",
			 "alternative_titles":{"titles":[{"iso_3166_1":"US","title":"Dream"},{"iso_3166_1":"CN","title":"盗梦空间"}]}}`))
		case "/3/tv/3":
			// a Chinese show: its own title is the localized one
			writeJSON(w, []byte(`{"id":3,"name":"繁花","original_name":"繁花","original_language":"zh",
			 "alternative_titles":{"results":[{"iso_3166_1":"CN","title":"别名"}]}}`))
		case "/3/tv/4":
			// no alternative for the region: the original stands
			writeJSON(w, []byte(`{"id":4,"name":"Some Show","original_name":"Some Show","original_language":"en",
			 "alternative_titles":{"results":[{"iso_3166_1":"DE","title":"Eine Serie"}]}}`))
		default:
			http.NotFound(w, r)
		}
	})
	cfg := fastConfig(map[string]string{"tmdb": srv.URL + "/3"})
	cfg.TMDBToken = fakeBearer
	cfg.Language = "zh-CN"
	set, _ := NewSet(cfg)

	for _, c := range []struct{ id, kind, want string }{
		{"tv/86831", "tv", "爱，死亡和机器人"},
		{"tv/1", "tv", "权力的游戏"},
		{"movie/2", "movie", "盗梦空间"},
		{"tv/3", "tv", "繁花"},
		{"tv/4", "tv", "Some Show"},
	} {
		tg := &ScrapeTarget{Kind: c.kind}
		tg.setID(ProviderTMDB, c.id)
		if err := set.TMDB.Fill(context.Background(), tg); err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		if tg.Title != c.want {
			t.Errorf("%s: title %q, want %q", c.id, tg.Title, c.want)
		}
	}
}
