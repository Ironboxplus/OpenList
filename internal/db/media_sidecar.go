package db

import (
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"gorm.io/gorm"
)

// ReplaceMediaSidecars makes rows the complete sidecar list of the item.
func ReplaceMediaSidecars(itemID uint, rows []model.MediaSidecar) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("item_id = ?", itemID).Delete(&model.MediaSidecar{}).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for i := range rows {
			rows[i].ID, rows[i].ItemID = 0, itemID
		}
		return tx.CreateInBatches(&rows, 200).Error
	})
}

// ListMediaSidecars returns the sidecars of the items, keyed by item id.
func ListMediaSidecars(itemIDs []uint) (map[uint][]model.MediaSidecar, error) {
	out := map[uint][]model.MediaSidecar{}
	for start := 0; start < len(itemIDs); start += 500 {
		end := min(start+500, len(itemIDs))
		var rows []model.MediaSidecar
		if err := db.Where("item_id IN ?", itemIDs[start:end]).Order("video, path").Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.ItemID] = append(out[r.ItemID], r)
		}
	}
	return out, nil
}
