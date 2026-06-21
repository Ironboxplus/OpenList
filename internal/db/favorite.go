package db

import (
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

func CreateFavorite(f *model.Favorite) error {
	return errors.WithStack(db.Create(f).Error)
}

func UpdateFavorite(f *model.Favorite) error {
	return errors.WithStack(db.Save(f).Error)
}

// DeleteFavorite removes a favorite scoped to the owning user so users can't
// delete favorites belonging to others.
func DeleteFavorite(userID, id uint) error {
	return errors.WithStack(
		db.Where("user_id = ? AND id = ?", userID, id).Delete(&model.Favorite{}).Error,
	)
}

func GetFavoritesByUser(userID uint) ([]model.Favorite, error) {
	var favorites []model.Favorite
	if err := db.Where("user_id = ?", userID).Order(columnName("created_at") + " DESC").Find(&favorites).Error; err != nil {
		return nil, errors.Wrapf(err, "failed get favorites")
	}
	return favorites, nil
}

// GetFavoriteByUserAndPath looks up a single favorite by (userID, path) for
// dedup/toggle. Returns (nil, nil) when no matching favorite exists.
func GetFavoriteByUserAndPath(userID uint, path string) (*model.Favorite, error) {
	var f model.Favorite
	if err := db.Where("user_id = ? AND path = ?", userID, path).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, errors.Wrapf(err, "failed get favorite")
	}
	return &f, nil
}
