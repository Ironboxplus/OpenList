package bootstrap

import (
	"context"
	"sync"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

func LoadStorages() {
	storages, err := db.GetEnabledStorages()
	if err != nil {
		utils.Log.Fatalf("failed get enabled storages: %+v", err)
	}

	// Seed the progress tracker so the status API reports work from the very
	// first poll, before any storage has finished loading.
	targets := make([]op.LoadTarget, len(storages))
	for i := range storages {
		targets[i] = op.LoadTarget{MountPath: storages[i].MountPath, Driver: storages[i].Driver}
	}
	op.StorageLoadProgress.Begin(targets)

	// Group by driver, preserving order. Storages of the SAME driver must load
	// sequentially — they often share a rate limiter, token-refresh mutex or
	// login endpoint, so concurrent init of the same driver type races. Different
	// driver types are independent and load in parallel.
	groupOrder := make([]string, 0)
	groups := make(map[string][]model.Storage)
	for i := range storages {
		d := storages[i].Driver
		if _, ok := groups[d]; !ok {
			groupOrder = append(groupOrder, d)
		}
		groups[d] = append(groups[d], storages[i])
	}

	go func() {
		var wg sync.WaitGroup
		for _, d := range groupOrder {
			wg.Add(1)
			go func(group []model.Storage) {
				defer wg.Done()
				for _, storage := range group {
					op.StorageLoadProgress.SetState(storage.MountPath, op.LoadLoading, "")
					err := op.LoadStorage(context.Background(), storage)
					if err != nil {
						op.StorageLoadProgress.SetState(storage.MountPath, op.LoadFailed, err.Error())
						utils.Log.Errorf("failed load storage: [%s], driver: [%s]: %+v",
							storage.MountPath, storage.Driver, err)
					} else {
						op.StorageLoadProgress.SetState(storage.MountPath, op.LoadLoaded, "")
						utils.Log.Infof("success load storage: [%s], driver: [%s], order: [%d]",
							storage.MountPath, storage.Driver, storage.Order)
					}
				}
			}(groups[d])
		}
		wg.Wait()
		conf.SendStoragesLoadedSignal()
	}()
}
