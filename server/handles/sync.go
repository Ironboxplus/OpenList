package handles

import (
	"strconv"

	"github.com/OpenListTeam/OpenList/v4/internal/syncjob"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// SyncRun starts a copy / sync / bisync / check job. Admin only. Errors in the
// request itself (bad mode, unmounted path, relative local dir, bad filter)
// come back here; errors during the run are reported by SyncGet.
func SyncRun(c *gin.Context) {
	var spec syncjob.Spec
	if err := c.ShouldBindJSON(&spec); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	snap, err := syncjob.Default.Start(spec)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, snap)
}

// SyncGet reports one job; events=true adds its per-file log.
func SyncGet(c *gin.Context) {
	snap, ok := syncjob.Default.Get(c.Query("id"), c.Query("events") == "true")
	if !ok {
		common.ErrorStrResp(c, "sync job not found", 404)
		return
	}
	common.SuccessResp(c, snap)
}

// SyncList reports the jobs held in memory: running ones and recent results.
func SyncList(c *gin.Context) {
	common.SuccessResp(c, syncjob.Default.List())
}

// SyncHistory reports finished jobs from the history file, newest first,
// including those from before the last restart.
func SyncHistory(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	hist, err := syncjob.Default.History(limit)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, hist)
}

func SyncCancel(c *gin.Context) {
	if !syncjob.Default.Cancel(c.Query("id")) {
		common.ErrorStrResp(c, "sync job not found", 404)
		return
	}
	common.SuccessResp(c)
}

// SyncBwLimit reads, or with a "rate" body sets, the bandwidth cap shared by
// all sync jobs. Changes apply to running jobs at once.
func SyncBwLimit(c *gin.Context) {
	if c.Request.Method == "POST" {
		var req struct {
			Rate string `json:"rate"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		if err := syncjob.SetBwLimit(req.Rate); err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
	}
	bps, err := syncjob.BwLimit()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, gin.H{"bytes_per_second": bps})
}
