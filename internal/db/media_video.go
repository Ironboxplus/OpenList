package db

import (
	"errors"
	stdpath "path"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A video work is identified by its path: the folder it sits in (FolderPath)
// and its own name (FileName, a folder name for a show or a collection). The
// functions below take and give that path.

// SplitMediaPath splits a work path into the FolderPath and FileName that
// identify it.
func SplitMediaPath(p string) (folder, name string) {
	p = stdpath.Clean("/" + p)
	return stdpath.Dir(p), stdpath.Base(p)
}

// MediaItemPath is the path of an item: where its file or folder lives.
func MediaItemPath(it *model.MediaItem) string {
	return stdpath.Join("/", it.FolderPath, it.FileName)
}

// GetVideoItemByPath returns the video work at p, or nil.
func GetVideoItemByPath(p string) (*model.MediaItem, error) {
	folder, name := SplitMediaPath(p)
	var it model.MediaItem
	err := db.Where("media_type = ? AND folder_path = ? AND file_name = ? AND album_name = ?",
		model.MediaTypeVideo, folder, name, "").First(&it).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// videoUnder limits a query to the video works at prefix and below it.
func videoUnder(tx *gorm.DB, prefix string) *gorm.DB {
	tx = tx.Where("media_type = ?", model.MediaTypeVideo)
	prefix = stdpath.Clean("/" + prefix)
	if prefix == "/" {
		return tx
	}
	folder, name := SplitMediaPath(prefix)
	return tx.Where(`(folder_path = ? AND file_name = ?) OR folder_path = ? OR folder_path LIKE ? ESCAPE '\'`,
		folder, name, prefix, escapeLike(prefix+"/")+"%")
}

// ListVideoItemsUnder returns the video works at prefix and below.
func ListVideoItemsUnder(prefix string) ([]model.MediaItem, error) {
	var items []model.MediaItem
	err := videoUnder(db.Model(&model.MediaItem{}), prefix).Order("folder_path, file_name").Find(&items).Error
	return items, err
}

// VideoListFilter narrows ListVideoItems.
type VideoListFilter struct {
	Prefix  string
	Kinds   []string // video types; empty = all
	Pending bool     // unlocked works whose confidence is below model.MediaPendingConfidence
	Page    int
	PerPage int
}

// ListVideoItems pages through the video works under a prefix, by path.
func ListVideoItems(f VideoListFilter) ([]model.MediaItem, int64, error) {
	tx := videoUnder(db.Model(&model.MediaItem{}), f.Prefix)
	if len(f.Kinds) > 0 {
		tx = tx.Where("video_type IN ?", f.Kinds)
	}
	if f.Pending {
		tx = tx.Where("locked = ? AND confidence < ?", false, model.MediaPendingConfidence)
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if f.PerPage <= 0 || f.PerPage > 500 {
		f.PerPage = 100
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	var items []model.MediaItem
	err := tx.Order("folder_path, file_name").Offset((f.Page - 1) * f.PerPage).Limit(f.PerPage).Find(&items).Error
	return items, total, err
}

// SaveVideoItem writes a work together with its ratings and episodes in one
// transaction. A nil ratings or eps leaves the stored set as it is; an empty
// one replaces it with nothing.
func SaveVideoItem(it *model.MediaItem, ratings []model.MediaRating, eps []model.MediaEpisode) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Save(it).Error; err != nil {
			return err
		}
		if ratings != nil {
			if err := tx.Where("item_id = ?", it.ID).Delete(&model.MediaRating{}).Error; err != nil {
				return err
			}
			for i := range ratings {
				ratings[i].ID, ratings[i].ItemID = 0, it.ID
			}
			if len(ratings) > 0 {
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&ratings).Error; err != nil {
					return err
				}
			}
		}
		if eps != nil {
			if err := tx.Where("item_id = ?", it.ID).Delete(&model.MediaEpisode{}).Error; err != nil {
				return err
			}
			for i := range eps {
				eps[i].ID, eps[i].ItemID = 0, it.ID
			}
			if len(eps) > 0 {
				if err := tx.CreateInBatches(&eps, 200).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// DeleteVideoItemsUnder deletes the video works at prefix and below, with their
// ratings, episodes and annotations. It reports how many works went.
func DeleteVideoItemsUnder(prefix string) (int64, error) {
	var n int64
	err := db.Transaction(func(tx *gorm.DB) error {
		var ids []uint
		if err := videoUnder(tx.Unscoped().Model(&model.MediaItem{}), prefix).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		res := tx.Unscoped().Where("id IN ?", ids).Delete(&model.MediaItem{})
		if res.Error != nil {
			return res.Error
		}
		n = res.RowsAffected
		return deleteMediaChildren(tx, ids)
	})
	return n, err
}

// DeleteVideoItemByPath deletes the single work at p (not what is below it).
func DeleteVideoItemByPath(p string) (int64, error) {
	it, err := GetVideoItemByPath(p)
	if err != nil || it == nil {
		return 0, err
	}
	return 1, DeleteMediaItem(it.ID)
}
