package handles

import (
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/session"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// SessionResp is one login session ("device") as the UI lists it.
type SessionResp struct {
	SessionID  string `json:"session_id"`
	UserID     uint   `json:"user_id,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	LastActive int64  `json:"last_active"`
	UA         string `json:"ua"`
	IP         string `json:"ip"`
	Current    bool   `json:"current"`
}

func sessionResps(list []model.Session, current string, withUser bool) []SessionResp {
	resp := make([]SessionResp, len(list))
	for i, s := range list {
		resp[i] = SessionResp{
			SessionID: s.SessionID, ClientID: s.ClientID, CreatedAt: s.CreatedAt, LastActive: s.LastActive,
			UA: s.UserAgent, IP: s.IP, Current: s.SessionID == current,
		}
		if withUser {
			resp[i].UserID = s.UserID
		}
	}
	return resp
}

func currentSessionID(c *gin.Context) string {
	id, _ := c.Request.Context().Value(conf.SessionIDKey).(string)
	return id
}

// ListMySessions lists the caller's own devices.
func ListMySessions(c *gin.Context) {
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	list, err := session.List(user.ID)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, sessionResps(list, currentSessionID(c), false))
}

type EvictSessionReq struct {
	SessionID string `json:"session_id" binding:"required"`
}

// EvictMySession signs one of the caller's devices out. Evicting the current
// session is a logout of this device.
func EvictMySession(c *gin.Context) {
	var req EvictSessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	ok, err := session.Revoke(user.ID, req.SessionID)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if !ok {
		common.ErrorStrResp(c, "session not found", 400)
		return
	}
	common.SuccessResp(c)
}

// ListSessions lists the devices of every user (admin).
func ListSessions(c *gin.Context) {
	list, err := session.List(0)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, sessionResps(list, currentSessionID(c), true))
}

// EvictSession signs any device out (admin).
func EvictSession(c *gin.Context) {
	var req EvictSessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	ok, err := session.Revoke(0, req.SessionID)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if !ok {
		common.ErrorStrResp(c, "session not found", 400)
		return
	}
	common.SuccessResp(c)
}
