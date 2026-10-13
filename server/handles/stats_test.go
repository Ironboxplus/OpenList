package handles

import (
	"encoding/json"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
)

func TestStatsOverviewAdminOnlyAndShape(t *testing.T) {
	m := newMediaEnv(t)
	// a library with a movie and a show with episodes
	for _, it := range []*model.MediaItem{
		{Path: "/lib/m", Kind: model.MediaKindMovie},
		{Path: "/lib/s", Kind: model.MediaKindTV},
	} {
		var eps []model.MediaEpisode
		if it.Kind == model.MediaKindTV {
			eps = []model.MediaEpisode{{FileName: "1.mkv", Season: 1, Episode: 1}}
		}
		if err := db.SaveMediaItem(it, eps); err != nil {
			t.Fatal(err)
		}
	}

	for _, who := range []string{"alice", "bob", "guest"} {
		env, w := m.call("GET", "/api/admin/stats/overview", who, nil)
		if w.Code == 200 && env.Code == 200 {
			t.Fatalf("%s must not read the overview, got %d/%d", who, w.Code, env.Code)
		}
		if env.Code != 403 {
			t.Fatalf("%s: code %d, want 403", who, env.Code)
		}
	}

	raw := m.ok("GET", "/api/admin/stats/overview", "admin", nil)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"node", "traffic", "media"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing %q in %s", k, raw)
		}
	}
	var traffic map[string]json.RawMessage
	if err := json.Unmarshal(got["traffic"], &traffic); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"now_bps", "series", "today", "month", "total", "daily"} {
		if _, ok := traffic[k]; !ok {
			t.Fatalf("missing traffic.%s in %s", k, got["traffic"])
		}
	}
	var today map[string]int64
	if err := json.Unmarshal(traffic["today"], &today); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"server_download", "server_upload", "client_download", "client_upload"} {
		if _, ok := today[k]; !ok {
			t.Fatalf("missing today.%s", k)
		}
	}
	var media map[string]int64
	if err := json.Unmarshal(got["media"], &media); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"movie", "tv", "anime", "collection", "episodes", "unscraped"} {
		if _, ok := media[k]; !ok {
			t.Fatalf("missing media.%s in %s", k, got["media"])
		}
	}
}
