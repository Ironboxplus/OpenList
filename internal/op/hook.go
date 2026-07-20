package op

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

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

// StorageCredentialEvent binds an authentication result to the exact Addition
// snapshot used by that request. A storage may be reinitialized with a peer
// credential while an earlier request is still in flight; listeners must never
// let that older result change the newer pair's cluster state.
type StorageCredentialEvent struct {
	Storage  driver.Driver
	Addition string
	Modified time.Time
}

type StorageCredentialHook func(typ string, event StorageCredentialEvent)
type StorageCredentialHealthHook func(event StorageCredentialEvent)

var (
	storageHooks                 = make([]StorageHook, 0)
	storageHealthHooks           = make([]StorageHealthHook, 0)
	storageCredentialHooks       = make([]StorageCredentialHook, 0)
	storageCredentialHealthHooks = make([]StorageCredentialHealthHook, 0)
	storageTokenStatusMu         sync.Mutex
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

// RegisterStorageCredentialHook receives token-valid/token-invalid events
// accompanied by the immutable Addition snapshot used on the wire.
func RegisterStorageCredentialHook(hook StorageCredentialHook) {
	storageCredentialHooks = append(storageCredentialHooks, hook)
}

// RegisterStorageCredentialHealthHook receives authenticated-success proofs
// accompanied by the exact Addition snapshot that succeeded.
func RegisterStorageCredentialHealthHook(hook StorageCredentialHealthHook) {
	storageCredentialHealthHooks = append(storageCredentialHealthHooks, hook)
}

func NotifyStorageTokenHealthy(storage driver.Driver) {
	for _, hook := range storageHealthHooks {
		hook(storage)
	}
}

// NotifyStorageTokenHealthyWithAddition is the generation-safe version used by
// drivers whose client can outlive a storage reconfiguration.
func NotifyStorageTokenHealthyWithAddition(storage driver.Driver, addition string) {
	st := storage.GetStorage()
	if st == nil {
		return
	}
	NotifyStorageTokenHealthyWithSnapshot(storage, addition, st.Modified)
}

// NotifyStorageTokenHealthyWithSnapshot rejects a late success from an older
// storage generation before it can renew local cluster-health proof.
func NotifyStorageTokenHealthyWithSnapshot(storage driver.Driver, addition string, modified time.Time) {
	if !storageSnapshotMatches(storage, addition, modified) {
		return
	}
	event := StorageCredentialEvent{Storage: storage, Addition: addition, Modified: modified}
	for _, hook := range storageCredentialHealthHooks {
		hook(event)
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
	if st == nil {
		return
	}
	NotifyStorageTokenInvalidWithAddition(storage, st.Addition)
}

// NotifyStorageTokenInvalidWithAddition records a 401 result only while the
// driver still serves the exact pair that made the failed request. A late 401
// from an old driver generation is therefore harmless after peer recovery has
// installed a newer pair.
func NotifyStorageTokenInvalidWithAddition(storage driver.Driver, addition string) {
	st := storage.GetStorage()
	if st == nil {
		return
	}
	NotifyStorageTokenInvalidWithSnapshot(storage, addition, st.Modified)
}

// NotifyStorageTokenInvalidWithSnapshot records a 401 only while both the
// in-memory storage and its durable row still match the request generation.
func NotifyStorageTokenInvalidWithSnapshot(storage driver.Driver, addition string, modified time.Time) {
	st := storage.GetStorage()
	if st == nil || st.Disabled || !storageSnapshotMatches(storage, addition, modified) {
		return
	}
	storageTokenStatusMu.Lock()
	recorded := markStorageTokenInvalidStatus(storage, addition, modified)
	storageTokenStatusMu.Unlock()
	if !recorded {
		return
	}
	// Persisting status is edge-triggered, but recovery is not. A newly adopted
	// remote candidate can fail while this mount is already marked invalid; that
	// failure must still reach cluster recovery, which performs its own per-mount
	// queue and candidate de-duplication.
	go callStorageHooks("token-invalid", storage)
	event := StorageCredentialEvent{Storage: storage, Addition: addition, Modified: modified}
	for _, hook := range storageCredentialHooks {
		hook := hook
		go hook("token-invalid", event)
	}
}

func markStorageTokenInvalidStatus(storage driver.Driver, addition string, modified time.Time) bool {
	st := storage.GetStorage()
	if st == nil || st.Disabled || !storageSnapshotMatches(storage, addition, modified) {
		return false
	}
	const invalidStatus = "token invalid"
	if st.ID == 0 {
		st.SetStatus(invalidStatus)
		return true
	}
	updated, err := db.UpdateStorageStatusIfModified(st.ID, modified, invalidStatus)
	if err != nil {
		log.Errorf("failed mark storage token invalid: %s", err)
		return false
	}
	if !updated {
		return false
	}
	// The conditional database update proves the durable row still belongs to
	// this request generation. Re-check memory before changing its status too.
	if !storageSnapshotMatches(storage, addition, modified) {
		return false
	}
	st.SetStatus(invalidStatus)
	return true
}

// NotifyStorageTokenValid signals that a storage's credentials were just proven
// good by a successful authenticated request. It restores a stale failure status
// before firing the storage hook, so listeners — notably cluster sync — can
// (re)share the proven token with peers. Drivers should call this only on a real
// transition or stale-status recovery to avoid per-request churn.
func NotifyStorageTokenValid(storage driver.Driver) {
	st := storage.GetStorage()
	if st == nil {
		return
	}
	NotifyStorageTokenValidWithAddition(storage, st.Addition)
}

// NotifyStorageTokenValidWithAddition restores status and emits a lifecycle
// event only when the authenticated request proved the pair currently mounted.
func NotifyStorageTokenValidWithAddition(storage driver.Driver, addition string) {
	st := storage.GetStorage()
	if st == nil {
		return
	}
	NotifyStorageTokenValidWithSnapshot(storage, addition, st.Modified)
}

// NotifyStorageTokenValidWithSnapshot restores status only for the exact
// request generation that completed successfully.
func NotifyStorageTokenValidWithSnapshot(storage driver.Driver, addition string, modified time.Time) {
	if !storageSnapshotMatches(storage, addition, modified) {
		return
	}
	storageTokenStatusMu.Lock()
	changed := restoreStorageTokenValidStatus(storage, addition, modified)
	storageTokenStatusMu.Unlock()
	if changed {
		go callStorageHooks("token-valid", storage)
	}
	// Credential proof is distinct from lifecycle status. A successful refresh
	// rotates the pair while the mount may remain WORK; cluster sync must still
	// receive and publish that newly proven generation.
	event := StorageCredentialEvent{Storage: storage, Addition: addition, Modified: modified}
	for _, hook := range storageCredentialHooks {
		hook := hook
		go hook("token-valid", event)
	}
}

func storageSnapshotMatches(storage driver.Driver, addition string, modified time.Time) bool {
	st := storage.GetStorage()
	return st != nil && st.Addition == addition && st.Modified.Equal(modified)
}

func restoreStorageTokenValidStatus(storage driver.Driver, addition string, modified time.Time) bool {
	st := storage.GetStorage()
	if st == nil || st.Disabled || st.Status == WORK || !storageSnapshotMatches(storage, addition, modified) {
		return false
	}
	if st.ID == 0 {
		st.SetStatus(WORK)
		return true
	}
	updated, err := db.UpdateStorageStatusIfModified(st.ID, modified, WORK)
	if err != nil {
		log.Errorf("failed mark storage token valid: %s", err)
		return false
	}
	if !updated || !storageSnapshotMatches(storage, addition, modified) {
		return false
	}
	st.SetStatus(WORK)
	return true
}
