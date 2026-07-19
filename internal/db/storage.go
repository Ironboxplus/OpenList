package db

import (
	"fmt"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/pkg/errors"
)

// why don't need `cache` for storage?
// because all storage store in `op.storagesMap`
// the most of the read operation is from `op.storagesMap`
// just for persistence in database

// CreateStorage just insert storage to database
func CreateStorage(storage *model.Storage) error {
	return errors.WithStack(db.Create(storage).Error)
}

// UpdateStorage just update storage in database
func UpdateStorage(storage *model.Storage) error {
	return errors.WithStack(db.Save(storage).Error)
}

// UpdateStorageStatus just updates storage status in database.
func UpdateStorageStatus(id uint, status string) error {
	return errors.WithStack(db.Model(&model.Storage{}).Where("id = ?", id).Update("status", status).Error)
}

// UpdateStorageStatusIfModified updates a lifecycle status only if the row is
// still the storage generation observed by the caller. This is used for auth
// callbacks: an old in-flight request must not mark a newly reconfigured token
// invalid after UpdateStorage has advanced Modified.
func UpdateStorageStatusIfModified(id uint, modified time.Time, status string) (bool, error) {
	tx := db.Model(&model.Storage{}).
		Where("id = ? AND modified = ?", id, modified).
		Update("status", status)
	if tx.Error != nil {
		return false, errors.WithStack(tx.Error)
	}
	return tx.RowsAffected > 0, nil
}

// DeleteStorageById just delete storage from database by id
func DeleteStorageById(id uint) error {
	return errors.WithStack(db.Delete(&model.Storage{}, id).Error)
}

// GetStorages Get all storages from database order by index
func GetStorages(pageIndex, pageSize int) ([]model.Storage, int64, error) {
	storageDB := db.Model(&model.Storage{})
	var count int64
	if err := storageDB.Count(&count).Error; err != nil {
		return nil, 0, errors.Wrapf(err, "failed get storages count")
	}
	var storages []model.Storage
	if err := addStorageOrder(storageDB).Order(columnName("order")).Offset((pageIndex - 1) * pageSize).Limit(pageSize).Find(&storages).Error; err != nil {
		return nil, 0, errors.WithStack(err)
	}
	return storages, count, nil
}

// GetStorageById Get Storage by id, used to update storage usually
func GetStorageById(id uint) (*model.Storage, error) {
	var storage model.Storage
	storage.ID = id
	if err := db.First(&storage).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return &storage, nil
}

// GetStorageByMountPath Get Storage by mountPath, used to update storage usually
func GetStorageByMountPath(mountPath string) (*model.Storage, error) {
	var storage model.Storage
	if err := db.Where("mount_path = ?", mountPath).First(&storage).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return &storage, nil
}

func GetEnabledStorages() ([]model.Storage, error) {
	var storages []model.Storage
	err := addStorageOrder(db).Where(fmt.Sprintf("%s = ?", columnName("disabled")), false).Find(&storages).Error
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return storages, nil
}
