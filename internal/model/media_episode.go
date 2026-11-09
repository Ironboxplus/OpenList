package model

import (
	"encoding/json"
	"path"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
)

// MediaEpisode maps one video file inside a series work (tv or anime) to its
// season and episode, with what the scraper knew about it. The same files are
// also listed in MediaItem.Episodes, which the upstream pages and the Emby API
// read; this table keeps the season/overview/still that JSON has no room for.
type MediaEpisode struct {
	ID       uint   `gorm:"primarykey"                                  json:"-"`
	ItemID   uint   `gorm:"not null;uniqueIndex:idx_media_episode_file" json:"item_id"`
	FileName string `gorm:"size:191;not null;uniqueIndex:idx_media_episode_file" json:"file_name"` // relative to the work's folder, e.g. "Season 1/x.S01E03.mkv"
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Title    string `json:"title"`
	Overview string `gorm:"type:text" json:"overview"`
	Still    string `json:"still"` // same spellings as MediaItem.Cover
	// Version marks a further file of an episode that another row of the same
	// season and number already provides (a second release folder).
	Version bool `json:"version,omitempty"`
}

// EpisodeInfos turns the episodes of a work into the upstream MediaItem.Episodes
// JSON. Index is the episode number when the work has one season; with several
// seasons the episode numbers repeat, so Index is the position in season and
// episode order instead (user data and the Emby ids key on it).
func EpisodeInfos(eps []MediaEpisode) []EpisodeInfo {
	sorted := append([]MediaEpisode(nil), eps...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Season != sorted[j].Season {
			return sorted[i].Season < sorted[j].Season
		}
		if sorted[i].Episode != sorted[j].Episode {
			return sorted[i].Episode < sorted[j].Episode
		}
		if sorted[i].Version != sorted[j].Version {
			return !sorted[i].Version
		}
		return sorted[i].FileName < sorted[j].FileName
	})
	multi := false
	for _, e := range sorted {
		if e.Season != sorted[0].Season {
			multi = true
			break
		}
	}
	out := make([]EpisodeInfo, 0, len(sorted))
	pos := 0 // position among the episodes proper: versions do not take one
	for _, e := range sorted {
		if e.Version && len(out) > 0 {
			// shares the number of the episode it is a version of
			out = append(out, EpisodeInfo{FileName: e.FileName, Index: out[len(out)-1].Index, Title: e.Title, Version: true})
			continue
		}
		pos++
		idx := e.Episode
		if multi || idx <= 0 {
			idx = pos
		}
		out = append(out, EpisodeInfo{FileName: e.FileName, Index: idx, Title: e.Title})
	}
	return out
}

// EpisodesJSON is EpisodeInfos as the string MediaItem.Episodes stores; "" when
// there are no episodes.
func EpisodesJSON(eps []MediaEpisode) string {
	if len(eps) == 0 {
		return ""
	}
	b, err := json.Marshal(EpisodeInfos(eps))
	if err != nil {
		return ""
	}
	return string(b)
}

// VersionsJSON is the Episodes JSON of a movie folder that holds several
// versions of the film: one entry per file, default first, each marked
// "version" so that clients show a version picker instead of an episode list.
// Index is 1-based, so ?episode=N picks the N-th version. "" for fewer than two.
func VersionsJSON(files []string) string {
	if len(files) < 2 {
		return ""
	}
	out := make([]EpisodeInfo, 0, len(files))
	for i, f := range files {
		title := strings.TrimSuffix(f, path.Ext(f))
		out = append(out, EpisodeInfo{FileName: f, Index: i + 1, Title: title, Version: true})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// ItemVersions returns the version entries of an Episodes JSON; nil for
// episodes or when there are none.
func ItemVersions(episodes string) []EpisodeInfo {
	if !strings.Contains(episodes, `"version":true`) {
		return nil
	}
	var eps []EpisodeInfo
	if err := json.Unmarshal([]byte(episodes), &eps); err != nil || len(eps) == 0 || !eps[0].Version {
		return nil
	}
	return eps
}

// ItemPlayFile is the video that plays for a non-episodic folder item (a film)
// when no episode is chosen: the first (default) version, else the one video the
// Episodes JSON records. False when the folder's video is not known (nothing
// recorded, or several without a version mark: no guessing) and for a single
// file, whose own path is the video.
func ItemPlayFile(item *MediaItem) (EpisodeInfo, bool) {
	if item == nil || !item.IsFolder || item.Episodes == "" {
		return EpisodeInfo{}, false
	}
	if vs := ItemVersions(item.Episodes); len(vs) > 0 {
		return vs[0], true
	}
	var eps []EpisodeInfo
	if err := json.Unmarshal([]byte(item.Episodes), &eps); err != nil {
		return EpisodeInfo{}, false
	}
	if len(eps) == 1 && eps[0].FileName != "" {
		return eps[0], true
	}
	if len(eps) > 1 {
		log.Debugf("[media] folder %s records %d videos and no version mark, play file unknown", item.FileName, len(eps))
	}
	return EpisodeInfo{}, false
}
