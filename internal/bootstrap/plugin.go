package bootstrap

import (
	"context"
	"path/filepath"
	"time"

	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/plugin"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// pluginScanInterval is how often the manager polls the plugins dir for changes
// (hot-reload).
const pluginScanInterval = 30 * time.Second

// InitPlugins boots the plugin manager: it watches <data>/plugins for backend
// Go-source plugins and frontend JS plugins and keeps them loaded, wires the
// plugin capability functions, and bridges the host's lifecycle hooks into the
// plugin registry so plugins receive real events. Failure is non-fatal — the
// server runs fine without plugins.
func InitPlugins() {
	root := filepath.Join(flags.DataDir, "plugins")
	mgr, err := plugin.NewManager(context.Background(), root, "/api/plugin/asset")
	if err != nil {
		utils.Log.Warnf("[plugin] init failed, plugin support disabled: %v", err)
		return
	}
	plugin.Default = mgr

	// Capability wiring. These let plugins read/write settings without the
	// plugin package importing internal/op (which would create an import cycle).
	plugin.SettingGetter = func(key string) string {
		return op.GetSettingsMap()[key]
	}
	plugin.SettingSetter = func(key, value string) error {
		item, err := op.GetSettingItemByKey(key)
		if err != nil {
			// Unknown key → create a private setting owned by the plugin.
			item = &model.SettingItem{
				Key:   key,
				Type:  "string",
				Group: model.SINGLE,
				Flag:  model.PRIVATE,
			}
		}
		item.Value = value
		return op.SaveSettingItem(item)
	}

	// Bridge the host's existing lifecycle hooks into the plugin registry, so a
	// plugin can react to storage and directory changes without us having to
	// scatter Fire() calls through op. FireHook is a no-op when nobody listens.
	op.RegisterStorageHook(func(typ string, storageDriver driver.Driver) {
		hook := map[string]string{
			"add":    plugin.HookStorageCreated,
			"del":    plugin.HookStorageDeleted,
			"update": plugin.HookStorageUpdated,
		}[typ]
		if hook == "" {
			return
		}
		st := storageDriver.GetStorage()
		plugin.FireHook(hook, map[string]any{
			"mount_path": st.MountPath,
			"driver":     st.Driver,
		})
	})
	op.RegisterObjsUpdateHook(func(_ context.Context, parent string, objs []model.Obj) {
		plugin.FireHook(plugin.HookFsObjsUpdated, map[string]any{
			"path":  parent,
			"count": len(objs),
		})
	})

	mgr.Start(pluginScanInterval)
	utils.Log.Infof("[plugin] manager started, watching %s", root)
}
