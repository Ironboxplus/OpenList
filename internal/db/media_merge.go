package db

import (
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ListVideoItemsInFolder returns every video work whose folder_path is folder,
// hidden ones included (a work folded into a sibling stays as a hidden row).
func ListVideoItemsInFolder(folder string) ([]model.MediaItem, error) {
	var items []model.MediaItem
	err := db.Where("media_type = ? AND folder_path = ?", model.MediaTypeVideo, folder).Order("id asc").Find(&items).Error
	return items, err
}

// FoldMediaItems makes the works in from part of the work to: they are hidden,
// not deleted, so that a rescan keeps finding their rows and does not bring them
// back, and their users' favourites and progress move to the survivor.
func FoldMediaItems(to uint, from []uint) error {
	if len(from) == 0 {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.MediaItem{}).Where("id IN ?", from).UpdateColumn("hidden", true).Error; err != nil {
			return err
		}
		var rows []model.MediaUserData
		if err := tx.Where("item_id IN ?", from).Find(&rows).Error; err != nil {
			return err
		}
		for _, r := range rows {
			// the survivor's own row for the same user and episode wins; the
			// favourite flag survives either way
			var keep model.MediaUserData
			err := tx.Where("user_id = ? AND item_id = ? AND episode_index = ?", r.UserID, to, r.EpisodeIndex).First(&keep).Error
			switch {
			case err == nil:
				if r.IsFavorite && !keep.IsFavorite {
					if err := tx.Model(&keep).UpdateColumn("is_favorite", true).Error; err != nil {
						return err
					}
				}
			case err == gorm.ErrRecordNotFound:
				moved := r
				moved.ID, moved.ItemID = 0, to
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&moved).Error; err != nil {
					return err
				}
			default:
				return err
			}
		}
		return tx.Where("item_id IN ?", from).Delete(&model.MediaUserData{}).Error
	})
}
