package db

import (
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ReplaceMediaRatings makes ratings the complete set of the item's ratings.
func ReplaceMediaRatings(itemID uint, ratings []model.MediaRating) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("item_id = ?", itemID).Delete(&model.MediaRating{}).Error; err != nil {
			return err
		}
		if len(ratings) == 0 {
			return nil
		}
		for i := range ratings {
			ratings[i].ID, ratings[i].ItemID = 0, itemID
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&ratings).Error
	})
}

// ListMediaRatings returns the ratings of the given items, grouped by item.
func ListMediaRatings(itemIDs ...uint) (map[uint][]model.MediaRating, error) {
	out := map[uint][]model.MediaRating{}
	if len(itemIDs) == 0 {
		return out, nil
	}
	var rows []model.MediaRating
	if err := db.Where("item_id IN ?", itemIDs).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ItemID] = append(out[r.ItemID], r)
	}
	return out, nil
}

// deleteMediaChildren removes the rows that hang off media items.
func deleteMediaChildren(tx *gorm.DB, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	for _, m := range []any{&model.MediaAnnotation{}, &model.MediaRating{}, &model.MediaEpisode{}, &model.MediaSidecar{}} {
		if err := tx.Where("item_id IN ?", ids).Delete(m).Error; err != nil {
			return err
		}
	}
	return nil
}
