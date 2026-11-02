package db

import (
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"gorm.io/gorm"
)

// ReplaceMediaEpisodes makes eps the complete episode list of the item.
func ReplaceMediaEpisodes(itemID uint, eps []model.MediaEpisode) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("item_id = ?", itemID).Delete(&model.MediaEpisode{}).Error; err != nil {
			return err
		}
		if len(eps) == 0 {
			return nil
		}
		for i := range eps {
			eps[i].ID, eps[i].ItemID = 0, itemID
		}
		return tx.CreateInBatches(&eps, 200).Error
	})
}

// ListMediaEpisodes returns the item's episodes in season and episode order.
func ListMediaEpisodes(itemID uint) ([]model.MediaEpisode, error) {
	var eps []model.MediaEpisode
	err := db.Where("item_id = ?", itemID).Order("season, episode, file_name").Find(&eps).Error
	return eps, err
}
