package db

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// MediaLibraryStamp is a cheap fingerprint of what the strm storage in media
// mode builds its tree from: it differs after a work is added, removed, saved
// or its episodes or sidecars change.
type MediaLibraryStamp struct {
	Items    int64
	Updated  time.Time // the latest updated_at of any item
	Episodes int64
	Sidecars int64
}

// GetMediaLibraryStamp returns the fingerprint of the video and music works.
func GetMediaLibraryStamp() (MediaLibraryStamp, error) {
	var s MediaLibraryStamp
	types := []model.MediaType{model.MediaTypeVideo, model.MediaTypeMusic}
	if err := db.Model(&model.MediaItem{}).Where("media_type IN ?", types).Count(&s.Items).Error; err != nil {
		return s, err
	}
	var last []time.Time
	if err := db.Model(&model.MediaItem{}).Where("media_type IN ?", types).Order("updated_at desc").Limit(1).Pluck("updated_at", &last).Error; err != nil {
		return s, err
	}
	if len(last) > 0 {
		s.Updated = last[0]
	}
	if err := db.Model(&model.MediaEpisode{}).Count(&s.Episodes).Error; err != nil {
		return s, err
	}
	err := db.Model(&model.MediaSidecar{}).Count(&s.Sidecars).Error
	return s, err
}

// ListMediaItemsForStrm lists the video and music works that can have a strm
// file: not hidden, and not a collection or an ignored folder.
func ListMediaItemsForStrm() ([]model.MediaItem, error) {
	var items []model.MediaItem
	err := db.Where("media_type IN ?", []model.MediaType{model.MediaTypeVideo, model.MediaTypeMusic}).
		Where("(hidden = ? OR hidden IS NULL)", false).
		Where("COALESCE(video_type, '') NOT IN ?", []string{model.VideoTypeCollection, model.VideoTypeIgnore}).
		Order("id asc").Find(&items).Error
	return items, err
}

// ListMediaEpisodesForItems returns the episodes of the items, keyed by item id,
// each list in season and episode order.
func ListMediaEpisodesForItems(itemIDs []uint) (map[uint][]model.MediaEpisode, error) {
	out := map[uint][]model.MediaEpisode{}
	for start := 0; start < len(itemIDs); start += 500 {
		end := min(start+500, len(itemIDs))
		var rows []model.MediaEpisode
		if err := db.Where("item_id IN ?", itemIDs[start:end]).Order("season, episode, version, file_name").Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.ItemID] = append(out[r.ItemID], r)
		}
	}
	return out, nil
}
