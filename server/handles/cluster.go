package handles

import (
	"io"
	"net/http"

	"github.com/OpenListTeam/OpenList/v4/internal/cluster"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// ClusterSync is the peer-to-peer endpoint nodes use to exchange sealed,
// encrypted sync messages. It is intentionally unauthenticated at the HTTP layer:
// authentication and confidentiality come from the cluster pre-shared key (only
// PSK holders can seal/open the AEAD envelope). The body and response are raw
// sealed bytes, not JSON.
func ClusterSync(c *gin.Context) {
	m := cluster.Default
	if m == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 16<<20))
	if err != nil {
		c.Status(http.StatusBadRequest)
		return
	}
	reply, err := m.HandleSync(raw)
	if err != nil {
		// Do not leak crypto details; a wrong key / replay / disabled all map to
		// a generic rejection.
		c.Status(http.StatusForbidden)
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", reply)
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
