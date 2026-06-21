package handles

import (
	"errors"
	"strconv"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// applyUserUpdatePolicy enforces who may change what during a user update.
// Role can never change here. A non-admin editor (a user delegated the
// "manage user info" permission) may only edit a normal user's username and
// password; every privileged field (permission, role, base path, disabled,
// otp) is forced back to the existing value, and admins can't be edited by them.
func applyUserUpdatePolicy(existing, req *model.User, byAdmin bool) error {
	if existing.Role != req.Role {
		return errors.New("role can not be changed")
	}
	if byAdmin {
		return nil
	}
	if existing.IsAdmin() {
		return errors.New("you are not allowed to edit an admin user")
	}
	// Profile-only edit: keep all privileged fields, allow username/password.
	username, password := req.Username, req.Password
	*req = *existing
	req.Username = username
	req.Password = password
	return nil
}

func ListUsers(c *gin.Context) {
	var req model.PageReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Validate()
	log.Debugf("%+v", req)
	users, total, err := op.GetUsers(req.Page, req.PerPage)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, common.PageResp{
		Content: users,
		Total:   total,
	})
}

func CreateUser(c *gin.Context) {
	var req model.User
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if req.IsAdmin() || req.IsGuest() {
		common.ErrorStrResp(c, "admin or guest user can not be created", 400, true)
		return
	}
	req.SetPassword(req.Password)
	req.Password = ""
	req.Authn = "[]"
	if err := op.CreateUser(&req); err != nil {
		common.ErrorResp(c, err, 500, true)
	} else {
		common.SuccessResp(c)
	}
}

func UpdateUser(c *gin.Context) {
	var req model.User
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user, err := op.GetUserById(req.ID)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	editor, _ := c.Request.Context().Value(conf.UserKey).(*model.User)
	byAdmin := editor != nil && editor.IsAdmin()
	if err := applyUserUpdatePolicy(user, &req, byAdmin); err != nil {
		common.ErrorStrResp(c, err.Error(), 403)
		return
	}
	if req.Password == "" {
		req.PwdHash = user.PwdHash
		req.Salt = user.Salt
	} else {
		req.SetPassword(req.Password)
		req.Password = ""
	}
	if req.OtpSecret == "" {
		req.OtpSecret = user.OtpSecret
	}
	if req.Disabled && req.IsAdmin() {
		common.ErrorStrResp(c, "admin user can not be disabled", 400)
		return
	}
	if err := op.UpdateUser(&req); err != nil {
		common.ErrorResp(c, err, 500)
	} else {
		common.SuccessResp(c)
	}
}

func DeleteUser(c *gin.Context) {
	idStr := c.Query("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.DeleteUserById(uint(id)); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

func GetUser(c *gin.Context) {
	idStr := c.Query("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user, err := op.GetUserById(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, user)
}

func Cancel2FAById(c *gin.Context) {
	idStr := c.Query("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.Cancel2FAById(uint(id)); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

func DelUserCache(c *gin.Context) {
	username := c.Query("username")
	err := op.DelUserCache(username)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}
