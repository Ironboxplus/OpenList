package model

import (
	"encoding/json"
	"sort"
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
	for i, e := range sorted {
		idx := e.Episode
		if multi || idx <= 0 {
			idx = i + 1
		}
		title := e.Title
		out = append(out, EpisodeInfo{FileName: e.FileName, Index: idx, Title: title})
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
