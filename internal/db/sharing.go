package db

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils/random"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

func GetSharingById(id string) (*model.SharingDB, error) {
	s := model.SharingDB{ID: id}
	if err := db.Where(s).First(&s).Error; err != nil {
		return nil, errors.Wrapf(err, "failed get sharing")
	}
	return &s, nil
}

func GetSharings(pageIndex, pageSize int) (sharings []model.SharingDB, count int64, err error) {
	sharingDB := db.Model(&model.SharingDB{})
	if err := sharingDB.Count(&count).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get sharings count")
	}
	if err := sharingDB.Order(columnName("id")).Offset((pageIndex - 1) * pageSize).Limit(pageSize).Find(&sharings).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get find sharings")
	}
	return sharings, count, nil
}

func GetSharingsByCreatorId(creator uint, pageIndex, pageSize int) (sharings []model.SharingDB, count int64, err error) {
	sharingDB := db.Model(&model.SharingDB{})
	cond := model.SharingDB{CreatorId: creator}
	if err := sharingDB.Where(cond).Count(&count).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get sharings count")
	}
	if err := sharingDB.Where(cond).Order(columnName("id")).Offset((pageIndex - 1) * pageSize).Limit(pageSize).Find(&sharings).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get find sharings")
	}
	return sharings, count, nil
}

func CreateSharing(s *model.SharingDB) (string, error) {
	if s.ID == "" {
		id := random.String(8)
		for len(id) < 12 {
			old := model.SharingDB{
				ID: id,
			}
			if err := db.Where(old).First(&old).Error; err != nil {
				s.ID = id
				return id, errors.WithStack(db.Create(s).Error)
			}
			id += random.String(1)
		}
		return "", errors.New("failed find valid id")
	} else {
		query := model.SharingDB{ID: s.ID}
		if err := db.Where(query).First(&query).Error; err == nil {
			return "", errors.New("sharing already exist")
		}
		return s.ID, errors.WithStack(db.Create(s).Error)
	}
}

// UpdateSharing saves the creator-editable part of a share. The visit counters
// are owned by the access path (AddSharingStats) and are left out: a share that
// was cached before a visit must not roll them back when it is saved.
func UpdateSharing(s *model.SharingDB) error {
	return errors.WithStack(db.Omit("view_count", "download_count", "last_access_at").Save(s).Error)
}

// effectiveMaxSQL is SharingDB.EffectiveMaxAccessed in SQL.
const effectiveMaxSQL = "CASE WHEN max_accessed > 0 THEN max_accessed WHEN burn_after_read THEN 1 ELSE 0 END"

// TakeSharingSlot takes one download slot atomically, and records the moment
// the limit is reached in consumed_at. It reports whether a slot was taken.
func TakeSharingSlot(id string) (bool, error) {
	res := db.Model(&model.SharingDB{}).
		Where("id = ? AND ("+effectiveMaxSQL+" <= 0 OR accessed < "+effectiveMaxSQL+")", id).
		UpdateColumns(map[string]any{"accessed": gorm.Expr("accessed + 1"), "last_access_at": time.Now()})
	if err := res.Error; err != nil || res.RowsAffected == 0 {
		return false, errors.WithStack(err)
	}
	// Separate statement on purpose: in MySQL a SET clause sees the columns
	// already assigned to its left, so "accessed" must not be read and written
	// in the same UPDATE. The condition makes it idempotent.
	err := db.Model(&model.SharingDB{}).
		Where("id = ? AND consumed_at IS NULL AND "+effectiveMaxSQL+" > 0 AND accessed >= "+effectiveMaxSQL, id).
		UpdateColumn("consumed_at", time.Now()).Error
	return true, errors.WithStack(err)
}

// AddSharingStats adds to the visit counters and stamps last_access_at.
func AddSharingStats(id string, views, downloads int64) error {
	cols := map[string]any{"last_access_at": time.Now()}
	if views != 0 {
		cols["view_count"] = gorm.Expr("view_count + ?", views)
	}
	if downloads != 0 {
		cols["download_count"] = gorm.Expr("download_count + ?", downloads)
	}
	return errors.WithStack(db.Model(&model.SharingDB{}).Where("id = ?", id).UpdateColumns(cols).Error)
}

// migrateSharingPasswords gives every share that still has only a plaintext
// password its salted hash. The plaintext column is kept (phase 1): an older
// binary reads it, and the creator can still see the password. Idempotent: rows
// that already have a hash are never touched.
func migrateSharingPasswords() error {
	var rows []model.SharingDB
	if err := db.Where("pwd <> ? AND (password_hash IS NULL OR password_hash = ?)", "", "").Find(&rows).Error; err != nil {
		return errors.WithStack(err)
	}
	for i := range rows {
		s := &rows[i]
		s.SetPassword(s.Pwd)
		err := db.Model(&model.SharingDB{}).
			Where("id = ? AND (password_hash IS NULL OR password_hash = ?)", s.ID, "").
			UpdateColumns(map[string]any{"password_salt": s.PasswordSalt, "password_hash": s.PasswordHash}).Error
		if err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func UpdateSharingId(oldId, newId string) error {
	// Check if new ID already exists
	if err := db.Where("id = ?", newId).First(&model.SharingDB{}).Error; err == nil {
		return errors.New("sharing id already exists")
	}
	return errors.WithStack(db.Model(&model.SharingDB{}).Where("id = ?", oldId).Update("id", newId).Error)
}

func DeleteSharingById(id string) error {
	s := model.SharingDB{ID: id}
	return errors.WithStack(db.Where(s).Delete(&s).Error)
}

func DeleteSharingsByCreatorId(creatorId uint) error {
	return errors.WithStack(db.Where("creator_id = ?", creatorId).Delete(&model.SharingDB{}).Error)
}
