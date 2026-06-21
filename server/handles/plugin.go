package handles

import (
	"net/http"

	"github.com/OpenListTeam/OpenList/v4/internal/plugin"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// PluginManifest serves the list of frontend (JS) plugins for the web UI to
// hot-load. Always returns a (possibly empty) manifest.
func PluginManifest(c *gin.Context) {
	if plugin.Default == nil {
		common.SuccessResp(c, plugin.Manifest{Plugins: []plugin.ManifestEntry{}})
		return
	}
	common.SuccessResp(c, plugin.Default.FrontendManifest())
}

// PluginAsset serves a frontend plugin's JS file by name, guarding against path
// traversal.
func PluginAsset(c *gin.Context) {
	if plugin.Default == nil {
		c.Status(http.StatusNotFound)
		return
	}
	path, ok := plugin.Default.AssetPath(c.Param("name"))
	if !ok {
		c.Status(http.StatusBadRequest)
		return
	}
	c.File(path)
}

// ---- Admin management of backend (Go-source) plugins ----

// PluginListResp is the payload for the management UI: backend Go plugins plus
// the frontend JS plugins served to the web UI.
type PluginListResp struct {
	Go       []plugin.PluginInfo    `json:"go"`
	Frontend []plugin.ManifestEntry `json:"frontend"`
}

func pluginMgr(c *gin.Context) *plugin.Manager {
	if plugin.Default == nil {
		common.ErrorStrResp(c, "plugin support is disabled", 500)
		return nil
	}
	return plugin.Default
}

// PluginList returns all backend Go plugins (with load status) and frontend JS
// plugins. Admin only.
func PluginList(c *gin.Context) {
	mgr := pluginMgr(c)
	if mgr == nil {
		return
	}
	common.SuccessResp(c, PluginListResp{
		Go:       mgr.ListGoPlugins(),
		Frontend: mgr.FrontendManifest().Plugins,
	})
}

// PluginGet returns a backend plugin's source for editing. Admin only.
func PluginGet(c *gin.Context) {
	mgr := pluginMgr(c)
	if mgr == nil {
		return
	}
	src, err := mgr.GetGoPluginSource(c.Query("name"))
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	common.SuccessResp(c, gin.H{"name": c.Query("name"), "source": src})
}

type PluginSaveReq struct {
	Name   string `json:"name" binding:"required"`
	Source string `json:"source" binding:"required"`
}

// PluginSave creates or overwrites a backend plugin and loads it immediately.
// A successful write whose code fails to load still returns 200 — the load
// error is reported by PluginList. Admin only.
func PluginSave(c *gin.Context) {
	mgr := pluginMgr(c)
	if mgr == nil {
		return
	}
	var req PluginSaveReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := mgr.SaveGoPlugin(req.Name, req.Source); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, mgr.ListGoPlugins())
}

type PluginNameReq struct {
	Name string `json:"name" binding:"required"`
}

// PluginDelete removes a backend plugin and unloads it. Admin only.
func PluginDelete(c *gin.Context) {
	mgr := pluginMgr(c)
	if mgr == nil {
		return
	}
	var req PluginNameReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := mgr.DeleteGoPlugin(req.Name); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, mgr.ListGoPlugins())
}

type PluginEnableReq struct {
	Name    string `json:"name" binding:"required"`
	Enabled bool   `json:"enabled"`
}

// PluginSetEnabled enables/disables a backend plugin. Admin only.
func PluginSetEnabled(c *gin.Context) {
	mgr := pluginMgr(c)
	if mgr == nil {
		return
	}
	var req PluginEnableReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := mgr.SetGoPluginEnabled(req.Name, req.Enabled); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, mgr.ListGoPlugins())
}
