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
	err := db.Where("item_id = ?", itemID).Order("season, episode, version, file_name").Find(&eps).Error
	return eps, err
}

// SyncMediaEpisodes makes the item's episodes match the files a scan found.
// A file that is still there keeps the title, overview and still a scraper or a
// person gave it; new files are added; files that are gone are dropped.
func SyncMediaEpisodes(itemID uint, scanned []model.MediaEpisode) ([]model.MediaEpisode, error) {
	var out []model.MediaEpisode
	err := db.Transaction(func(tx *gorm.DB) error {
		var old []model.MediaEpisode
		if err := tx.Where("item_id = ?", itemID).Find(&old).Error; err != nil {
			return err
		}
		byFile := make(map[string]model.MediaEpisode, len(old))
		for _, e := range old {
			byFile[e.FileName] = e
		}
		out = make([]model.MediaEpisode, 0, len(scanned))
		for _, e := range scanned {
			if o, ok := byFile[e.FileName]; ok {
				if e.Title == "" {
					e.Title = o.Title
				}
				if e.Overview == "" {
					e.Overview = o.Overview
				}
				if e.Still == "" {
					e.Still = o.Still
				}
			}
			out = append(out, e)
		}
		if err := tx.Where("item_id = ?", itemID).Delete(&model.MediaEpisode{}).Error; err != nil {
			return err
		}
		if len(out) == 0 {
			return nil
		}
		for i := range out {
			out[i].ID, out[i].ItemID = 0, itemID
		}
		return tx.CreateInBatches(&out, 200).Error
	})
	return out, err
}
