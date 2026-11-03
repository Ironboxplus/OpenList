package db

import (
	stdpath "path"
	"strings"
	"unicode/utf8"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// videoClass groups video types by what a rescan treats as the same kind of
// work: a movie, a show (tv and anime), a collection, or something ignored.
func videoClass(videoType string) string {
	switch {
	case model.IsEpisodicVideo(videoType):
		return "show"
	case videoType == model.VideoTypeMovie, videoType == model.VideoTypeCollection, videoType == model.VideoTypeIgnore:
		return videoType
	}
	return ""
}

// SaveScannedMediaItem is CreateOrUpdateMediaItem for a video scan that knows
// whether its verdict can be trusted (the folder and the folders it looked
// into were listed without error).
//
// A scraped row that is neither locked nor manual takes the scan's video type
// when the scan is trusted, so a work the rules now read differently is no
// longer stuck with its old kind. Everything the scrape wrote (cover, plot,
// ratings, titles, backdrop) stays until a new scrape replaces it. When the
// class of the work changes (movie, show, collection, ignore) the row is also
// queued for a new scrape and drops what only fits the old class:
// scraped_at (NULL queues it), external_id and external_ids (their "tv/123" /
// "movie/123" is the wrong type now), the candidates, and the confidence and
// source, which become the scan's. A work that stops being a show loses its
// episode rows. Same class (tv to anime): only video_type changes.
//
// A scraped, unlocked, non-manual work that was scraped under an older
// model.MediaIdentifyVersion is queued for a new scrape too (scraped_at NULL),
// whatever the scan says about its kind: a better rule or search then reaches
// the matches the old one got wrong. Nothing else is dropped, so the cover and
// the data stay until the new scrape replaces them.
func SaveScannedMediaItem(item *model.MediaItem, trusted bool) error {
	if utf8.RuneCountInString(item.FolderPath) > model.MediaIndexColumnMax ||
		utf8.RuneCountInString(item.FileName) > model.MediaIndexColumnMax ||
		utf8.RuneCountInString(item.AlbumName) > model.MediaIndexColumnMax {
		return CreateOrUpdateMediaItem(item) // reports the error
	}
	var existing model.MediaItem
	res := db.Unscoped().Where("folder_path = ? AND file_name = ? AND album_name = ?", item.FolderPath, item.FileName, item.AlbumName).First(&existing)
	if res.Error != nil || existing.Locked || existing.Source == model.MediaSourceManual || existing.ScrapedAt == nil {
		return CreateOrUpdateMediaItem(item)
	}
	scan := *item
	if err := CreateOrUpdateMediaItem(item); err != nil {
		return err
	}
	stale := item.MediaType == model.MediaTypeVideo && existing.IdentifyVersion < model.MediaIdentifyVersion
	if !trusted || item.MediaType != model.MediaTypeVideo || scan.VideoType == "" || scan.VideoType == existing.VideoType {
		if stale {
			return db.Unscoped().Model(&model.MediaItem{}).Where("id = ?", item.ID).Update("scraped_at", nil).Error
		}
		return nil
	}
	oldClass, newClass := videoClass(existing.VideoType), videoClass(scan.VideoType)
	upd := map[string]any{"video_type": scan.VideoType}
	if stale {
		upd["scraped_at"] = nil
	}
	if oldClass != newClass {
		upd["season"], upd["episode"] = scan.Season, scan.Episode
		upd["scraped_at"] = nil
		upd["external_id"], upd["external_ids"], upd["candidates"] = "", "", ""
		upd["source"], upd["confidence"] = scan.Source, scan.Confidence
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Model(&model.MediaItem{}).Where("id = ?", item.ID).Updates(upd).Error; err != nil {
			return err
		}
		if oldClass == "show" && newClass != "show" {
			return tx.Where("item_id = ?", item.ID).Delete(&model.MediaEpisode{}).Error
		}
		return nil
	})
}

// PruneMediaItems soft-deletes the video rows of a scan path that the scan
// just finished did not produce, so a work the rules now read differently
// (a collection that is one film with versions) does not keep its old member
// rows. Rows the scan produced keep their id and with it their progress and
// favourites; a pruned row stays in the table, so a work that comes back (a
// storage that was away) gets its old id again.
//
// Never pruned: locked and manual rows, and anything at or below a path in
// failed (a listing, a peek or a save went wrong there: absence proves
// nothing). Nothing is pruned when the scan produced nothing, which is what a
// root that listed empty or failed looks like. It returns how many rows went.
func PruneMediaItems(scanPathID uint, produced map[uint]bool, failed []string) (int, error) {
	if len(produced) == 0 {
		return 0, nil
	}
	var rows []model.MediaItem
	err := db.Select("id", "folder_path", "file_name").
		Where("scan_path_id = ? AND media_type = ? AND locked = ? AND (source IS NULL OR source <> ?)",
			scanPathID, model.MediaTypeVideo, false, model.MediaSourceManual).
		Find(&rows).Error
	if err != nil {
		return 0, err
	}
	var gone []uint
	for i := range rows {
		r := &rows[i]
		p := stdpath.Join(r.FolderPath, r.FileName)
		if produced[r.ID] || underAny(p, failed) {
			continue
		}
		if len(gone) < 20 {
			log.Infof("media prune [scan path %d]: removing id %d %s", scanPathID, r.ID, p)
		}
		gone = append(gone, r.ID)
	}
	count := len(gone)
	for len(gone) > 0 {
		n := min(len(gone), 400)
		if err := db.Where("id IN ?", gone[:n]).Delete(&model.MediaItem{}).Error; err != nil {
			return 0, err
		}
		gone = gone[n:]
	}
	return count, nil
}

// underAny reports whether p is one of the paths or below one of them.
func underAny(p string, paths []string) bool {
	for _, f := range paths {
		f = stdpath.Clean("/" + f)
		if p == f || f == "/" || strings.HasPrefix(p, f+"/") {
			return true
		}
	}
	return false
}

// DecidedShowKind returns the video type of the stored work at folderPath and
// fileName when it is a show that a scraper or a person decided (source not
// "rule"), "" otherwise. A scan whose rules read such a folder as a collection
// of seasons or releases (星蝶公主: 纯英 and 中英) keeps the decision and does not
// open the folder into member rows again.
func DecidedShowKind(folderPath, fileName string) string {
	var it model.MediaItem
	err := db.Select("video_type", "source").
		Where("folder_path = ? AND file_name = ? AND album_name = ''", folderPath, fileName).First(&it).Error
	if err != nil || it.Source == model.MediaSourceRule || it.Source == "" || !model.IsEpisodicVideo(it.VideoType) {
		return ""
	}
	return it.VideoType
}
