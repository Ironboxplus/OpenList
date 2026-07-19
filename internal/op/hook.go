package op

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// Obj
type ObjsUpdateHook = func(ctx context.Context, parent string, objs []model.Obj)

var (
	objsUpdateHooks = make([]ObjsUpdateHook, 0)
)

func RegisterObjsUpdateHook(hook ObjsUpdateHook) {
	objsUpdateHooks = append(objsUpdateHooks, hook)
}

func HandleObjsUpdateHook(ctx context.Context, parent string, objs []model.Obj) {
	for _, hook := range objsUpdateHooks {
		hook(ctx, parent, objs)
	}
}

// Setting
type SettingItemHook func(item *model.SettingItem) error

var settingItemHooks = map[string]SettingItemHook{
	conf.VideoTypes: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.VideoTypes] = strings.Split(item.Value, ",")
		return nil
	},
	conf.AudioTypes: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.AudioTypes] = strings.Split(item.Value, ",")
		return nil
	},
	conf.ImageTypes: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.ImageTypes] = strings.Split(item.Value, ",")
		return nil
	},
	conf.TextTypes: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.TextTypes] = strings.Split(item.Value, ",")
		return nil
	},
	conf.ProxyTypes: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.ProxyTypes] = strings.Split(item.Value, ",")
		return nil
	},
	conf.ProxyIgnoreHeaders: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.ProxyIgnoreHeaders] = strings.Split(item.Value, ",")
		return nil
	},
	conf.PrivacyRegs: func(item *model.SettingItem) error {
		regStrs := strings.Split(item.Value, "\n")
		regs := make([]*regexp.Regexp, 0, len(regStrs))
		for _, regStr := range regStrs {
			reg, err := regexp.Compile(regStr)
			if err != nil {
				return errors.WithStack(err)
			}
			regs = append(regs, reg)
		}
		conf.PrivacyReg = regs
		return nil
	},
	conf.FilenameCharMapping: func(item *model.SettingItem) error {
		err := utils.Json.UnmarshalFromString(item.Value, &conf.FilenameCharMap)
		if err != nil {
			return err
		}
		log.Debugf("filename char mapping: %+v", conf.FilenameCharMap)
		return nil
	},
	conf.IgnoreDirectLinkParams: func(item *model.SettingItem) error {
		conf.SlicesMap[conf.IgnoreDirectLinkParams] = strings.Split(item.Value, ",")
		return nil
	},
}

func RegisterSettingItemHook(key string, hook SettingItemHook) {
	settingItemHooks[key] = hook
}

func HandleSettingItemHook(item *model.SettingItem) (hasHook bool, err error) {
	if hook, ok := settingItemHooks[item.Key]; ok {
		return true, hook(item)
	}
	return false, nil
}

// Storage
type StorageHook func(typ string, storage driver.Driver)
type StorageHealthHook func(storage driver.Driver)

var (
	storageHooks         = make([]StorageHook, 0)
	storageHealthHooks   = make([]StorageHealthHook, 0)
	storageTokenStatusMu sync.Mutex
)

func callStorageHooks(typ string, storage driver.Driver) {
	for _, hook := range storageHooks {
		hook(typ, storage)
	}
}

func RegisterStorageHook(hook StorageHook) {
	storageHooks = append(storageHooks, hook)
}

// RegisterStorageHealthHook receives a cheap proof that an authenticated
// request succeeded. It is distinct from StorageHook: callers use it for
// in-memory liveness only, never for database writes or lifecycle work.
func RegisterStorageHealthHook(hook StorageHealthHook) {
	storageHealthHooks = append(storageHealthHooks, hook)
}

func NotifyStorageTokenHealthy(storage driver.Driver) {
	for _, hook := range storageHealthHooks {
		hook(storage)
	}
}

// NotifyStorageTokenInvalid signals that a storage's credentials were found to be
// invalid/expired while in use (as opposed to successfully refreshed). It fires
// the storage hook with the "token-invalid" type so listeners — notably cluster
// sync — can react by pulling fresh credentials from a healthy peer instead of
// propagating the broken token. Drivers may call this when an API call fails with
// an unrecoverable auth error.
func NotifyStorageTokenInvalid(storage driver.Driver) {
	st := storage.GetStorage()
	if st == nil || st.Disabled {
		return
	}
	storageTokenStatusMu.Lock()
	_ = markStorageTokenInvalidStatus(storage)
	storageTokenStatusMu.Unlock()
	// Persisting status is edge-triggered, but recovery is not. A newly adopted
	// remote candidate can fail while this mount is already marked invalid; that
	// failure must still reach cluster recovery, which performs its own per-mount
	// cooldown and candidate de-duplication.
	go callStorageHooks("token-invalid", storage)
}

func markStorageTokenInvalidStatus(storage driver.Driver) bool {
	st := storage.GetStorage()
	if st == nil || st.Disabled || st.Status != WORK {
		return false
	}
	const invalidStatus = "token invalid"
	st.SetStatus(invalidStatus)
	if st.ID == 0 {
		return true
	}
	if err := db.UpdateStorageStatus(st.ID, invalidStatus); err != nil {
		log.Errorf("failed mark storage token invalid: %s", err)
	}
	return true
}

// NotifyStorageTokenValid signals that a storage's credentials were just proven
// good by a successful authenticated request. It restores a stale failure status
// before firing the storage hook, so listeners — notably cluster sync — can
// (re)share the proven token with peers. Drivers should call this only on a real
// transition or stale-status recovery to avoid per-request churn.
func NotifyStorageTokenValid(storage driver.Driver) {
	storageTokenStatusMu.Lock()
	changed := restoreStorageTokenValidStatus(storage)
	storageTokenStatusMu.Unlock()
	if changed {
		go callStorageHooks("token-valid", storage)
	}
}

func restoreStorageTokenValidStatus(storage driver.Driver) bool {
	st := storage.GetStorage()
	if st == nil || st.Disabled || st.Status == WORK {
		return false
	}
	st.SetStatus(WORK)
	if st.ID == 0 {
		return true
	}
	if err := db.UpdateStorageStatus(st.ID, WORK); err != nil {
		log.Errorf("failed mark storage token valid: %s", err)
	}
	return true
}
