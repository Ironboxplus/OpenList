package db

import (
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// mediaListColumns is the whitelist of columns the list endpoints read. The
// heavy ones (overview, candidates) are never selected for lists.
var mediaListColumns = []string{"id", "path", "kind", "title", "original_title", "year", "ratings", "poster_id", "source", "confidence", "locked", "candidates"}

// mediaChildColumns is what a directory view needs for each child.
var mediaChildColumns = []string{"id", "path", "kind", "title", "year", "ratings", "poster_id"}

// likeEscape escapes LIKE wildcards; the escape character is '!' because it
// means the same in every SQL dialect we support (a backslash does not).
func likeEscape(s string) string {
	s = strings.ReplaceAll(s, "!", "!!")
	s = strings.ReplaceAll(s, "%", "!%")
	return strings.ReplaceAll(s, "_", "!_")
}

// GetMediaItemByPath returns (nil, nil) when there is no item at path.
func GetMediaItemByPath(path string) (*model.MediaItem, error) {
	// Find+Limit rather than First: a missing item is normal and must not
	// log "record not found".
	var items []model.MediaItem
	if err := db.Where("path = ?", path).Limit(1).Find(&items).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	return &items[0], nil
}

// GetMediaItemsByPaths returns the items at exactly these paths.
func GetMediaItemsByPaths(paths []string) ([]model.MediaItem, error) {
	var items []model.MediaItem
	if len(paths) == 0 {
		return items, nil
	}
	if err := db.Where("path IN ?", paths).Find(&items).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return items, nil
}

// ListMediaChildren returns the items that sit directly under parent, with the
// list-column whitelist only.
func ListMediaChildren(parent string) ([]model.MediaItem, error) {
	prefix := strings.TrimRight(parent, "/") + "/"
	var items []model.MediaItem
	err := db.Select(mediaChildColumns).
		Where("path LIKE ? ESCAPE '!'", likeEscape(prefix)+"%").
		Where("path NOT LIKE ? ESCAPE '!'", likeEscape(prefix)+"%/%").
		Order("path").Find(&items).Error
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return items, nil
}

// MediaListFilter selects items for the admin list.
type MediaListFilter struct {
	Prefix  string // path or directory prefix; "" or "/" = everything
	Kind    string
	Pending bool
	Page    int
	PerPage int
}

func (f MediaListFilter) apply(q *gorm.DB) *gorm.DB {
	if p := strings.TrimRight(f.Prefix, "/"); p != "" {
		q = q.Where("(path = ? OR path LIKE ? ESCAPE '!')", p, likeEscape(p)+"/%")
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Pending {
		// not locked, and either low confidence or candidates waiting for a choice
		q = q.Where("locked = ?", false).
			Where("(confidence < ? OR (candidates <> '' AND candidates <> '[]' AND candidates <> 'null'))", model.MediaPendingConfidence)
	}
	return q
}

func ListMediaItems(f MediaListFilter) ([]model.MediaItem, int64, error) {
	var total int64
	if err := f.apply(db.Model(&model.MediaItem{})).Count(&total).Error; err != nil {
		return nil, 0, errors.WithStack(err)
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PerPage < 1 || f.PerPage > 1000 {
		f.PerPage = 100
	}
	var items []model.MediaItem
	err := f.apply(db.Select(mediaListColumns)).Order("path").
		Offset((f.Page - 1) * f.PerPage).Limit(f.PerPage).Find(&items).Error
	if err != nil {
		return nil, 0, errors.WithStack(err)
	}
	return items, total, nil
}

// AllMediaItems is for export.
func AllMediaItems(prefix string) ([]model.MediaItem, error) {
	var items []model.MediaItem
	if err := (MediaListFilter{Prefix: prefix}).apply(db.Model(&model.MediaItem{})).Order("path").Find(&items).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return items, nil
}

// SaveMediaItem inserts or updates (by primary key) the item, and when
// episodes is non-nil replaces its episodes, in one transaction.
func SaveMediaItem(it *model.MediaItem, episodes []model.MediaEpisode) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(it).Error; err != nil {
			return err
		}
		if episodes == nil {
			return nil
		}
		if err := tx.Where("item_id = ?", it.ID).Delete(&model.MediaEpisode{}).Error; err != nil {
			return err
		}
		for i := range episodes {
			episodes[i].ID = 0
			episodes[i].ItemID = it.ID
		}
		if len(episodes) == 0 {
			return nil
		}
		return tx.CreateInBatches(episodes, 200).Error
	}))
}

func GetMediaEpisodes(itemID uint) ([]model.MediaEpisode, error) {
	var eps []model.MediaEpisode
	if err := db.Where("item_id = ?", itemID).Order("season, episode, id").Find(&eps).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return eps, nil
}

// DeleteMediaItemByPath removes one item and its episodes; reports how many
// items went.
func DeleteMediaItemByPath(path string) (int64, error) {
	return deleteMediaItems(db.Where("path = ?", path))
}

// DeleteMediaItemsByPrefix removes the item at prefix and everything below it.
func DeleteMediaItemsByPrefix(prefix string) (int64, error) {
	p := strings.TrimRight(prefix, "/")
	if p == "" {
		return deleteMediaItems(db.Where("1 = 1"))
	}
	return deleteMediaItems(db.Where("(path = ? OR path LIKE ? ESCAPE '!')", p, likeEscape(p)+"/%"))
}

func deleteMediaItems(cond *gorm.DB) (int64, error) {
	var n int64
	err := db.Transaction(func(tx *gorm.DB) error {
		var ids []uint
		if err := tx.Model(&model.MediaItem{}).Where(cond).Pluck("id", &ids).Error; err != nil {
			return err
		}
		n = int64(len(ids))
		for i := 0; i < len(ids); i += 500 {
			end := min(i+500, len(ids))
			chunk := ids[i:end]
			if err := tx.Where("item_id IN ?", chunk).Delete(&model.MediaEpisode{}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", chunk).Delete(&model.MediaItem{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return n, errors.WithStack(err)
}

// ---- assets ----

func GetMediaAsset(id uint) (*model.MediaAsset, error) {
	var as []model.MediaAsset
	if err := db.Where("id = ?", id).Limit(1).Find(&as).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	if len(as) == 0 {
		return nil, nil
	}
	return &as[0], nil
}

func GetMediaAssetBySHA(sha string) (*model.MediaAsset, error) {
	var as []model.MediaAsset
	if err := db.Where("sha256 = ?", sha).Limit(1).Find(&as).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	if len(as) == 0 {
		return nil, nil
	}
	return &as[0], nil
}

func GetMediaAssets(ids []uint) ([]model.MediaAsset, error) {
	var as []model.MediaAsset
	if len(ids) == 0 {
		return as, nil
	}
	if err := db.Where("id IN ?", ids).Find(&as).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return as, nil
}

func CreateMediaAsset(a *model.MediaAsset) error {
	return errors.WithStack(db.Create(a).Error)
}
