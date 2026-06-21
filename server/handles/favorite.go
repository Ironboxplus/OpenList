package handles

import (
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// normalizeFavoritePath cleans a favorite path so that dedup/toggle compares
// equal paths consistently (backslashes, missing leading slash, ".."/"." are
// all normalized away).
func normalizeFavoritePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	return utils.FixAndCleanPath(path)
}

type FavoriteAddReq struct {
	Path  string `json:"path" binding:"required"`
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Tag   string `json:"tag"`
}

func currentUser(c *gin.Context) (*model.User, bool) {
	userObj, ok := c.Request.Context().Value(conf.UserKey).(*model.User)
	if !ok || userObj == nil {
		return nil, false
	}
	return userObj, true
}

func ListFavorites(c *gin.Context) {
	userObj, ok := currentUser(c)
	if !ok {
		common.ErrorStrResp(c, "user invalid", 401)
		return
	}
	favorites, err := db.GetFavoritesByUser(userObj.ID)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, favorites)
}

func AddFavorite(c *gin.Context) {
	userObj, ok := currentUser(c)
	if !ok {
		common.ErrorStrResp(c, "user invalid", 401)
		return
	}
	var req FavoriteAddReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorStrResp(c, "request invalid", 400)
		return
	}
	path := normalizeFavoritePath(req.Path)
	// dedup/toggle: if a favorite with the same (userID, path) exists, just
	// update its tag instead of creating a duplicate.
	existing, err := db.GetFavoriteByUserAndPath(userObj.ID, path)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	if existing != nil {
		existing.Tag = req.Tag
		existing.Name = req.Name
		existing.IsDir = req.IsDir
		if err := db.UpdateFavorite(existing); err != nil {
			common.ErrorResp(c, err, 500, true)
			return
		}
		common.SuccessResp(c, existing)
		return
	}
	favorite := &model.Favorite{
		UserID:    userObj.ID,
		Path:      path,
		Name:      req.Name,
		IsDir:     req.IsDir,
		Tag:       req.Tag,
		CreatedAt: time.Now().Unix(),
	}
	if err := db.CreateFavorite(favorite); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, favorite)
}

func DeleteFavorite(c *gin.Context) {
	userObj, ok := currentUser(c)
	if !ok {
		common.ErrorStrResp(c, "user invalid", 401)
		return
	}
	id, err := strconv.Atoi(c.Query("id"))
	if err != nil {
		common.ErrorStrResp(c, "id format invalid", 400)
		return
	}
	if err := db.DeleteFavorite(userObj.ID, uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}
