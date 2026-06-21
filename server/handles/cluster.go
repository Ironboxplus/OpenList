package handles

import (
	"net/http"

	"github.com/OpenListTeam/OpenList/v4/internal/cluster"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// ClusterWS is the peer-to-peer endpoint nodes use to establish a persistent,
// stateful sync connection (WebSocket). A persistent connection is required so a
// node behind NAT can participate: it dials OUT to this endpoint and keeps the
// link open, since it cannot be dialed itself. The HTTP upgrade is open;
// authentication and confidentiality come from the cluster pre-shared key (only
// PSK holders can seal/open the per-frame AEAD envelope).
func ClusterWS(c *gin.Context) {
	m := cluster.Default
	if m == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	m.ServeWS(c.Writer, c.Request)
}

// ---- Admin config/status ----

func clusterMgr(c *gin.Context) *cluster.Manager {
	if cluster.Default == nil {
		common.ErrorStrResp(c, "cluster sync is unavailable", 500)
		return nil
	}
	return cluster.Default
}

// ClusterGetConfig returns the current cluster config (key redacted) plus a live
// status overview. Admin only.
func ClusterGetConfig(c *gin.Context) {
	m := clusterMgr(c)
	if m == nil {
		return
	}
	common.SuccessResp(c, gin.H{
		"config": m.GetConfig(true),
		"status": m.Status(),
	})
}

// ClusterSetConfig persists a new cluster config. A blank or "********" key keeps
// the existing key. Admin only.
func ClusterSetConfig(c *gin.Context) {
	m := clusterMgr(c)
	if m == nil {
		return
	}
	var cfg cluster.Config
	if err := c.ShouldBind(&cfg); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := m.SetConfig(cfg); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, gin.H{
		"config": m.GetConfig(true),
		"status": m.Status(),
	})
}

// ClusterStatus returns just the live status overview. Admin only.
func ClusterStatus(c *gin.Context) {
	m := clusterMgr(c)
	if m == nil {
		return
	}
	common.SuccessResp(c, m.Status())
}

// ClusterSetGroups replaces the cluster-shared sync-group document. Admin only.
func ClusterSetGroups(c *gin.Context) {
	m := clusterMgr(c)
	if m == nil {
		return
	}
	var req struct {
		Groups []cluster.GroupSpec `json:"groups"`
	}
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := m.SetGroups(req.Groups); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, m.Status())
}
