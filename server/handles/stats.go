package handles

import (
	"github.com/OpenListTeam/OpenList/v4/internal/cluster"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/stats"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// StatsOverview answers /api/admin/stats/overview: the relayed traffic of this
// node (live speed, totals, the last 30 days) and the scraped library counts.
// Transfer and sync figures are not here; the transfer and sync APIs already
// serve them. Admin only (the route group enforces it).
func StatsOverview(c *gin.Context) {
	traffic, err := stats.Snapshot()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	// Same rule as /api/media/library: with media off there is nothing to count.
	var media db.MediaStats
	if mediaEnabled() {
		if media, err = db.GetMediaStats(); err != nil {
			common.ErrorResp(c, err, 500, true)
			return
		}
	}
	node := ""
	if cluster.Default != nil {
		node = cluster.Default.GetConfig(true).Label
	}
	common.SuccessResp(c, gin.H{"node": node, "traffic": traffic, "media": media})
}
