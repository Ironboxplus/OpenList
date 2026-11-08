package handles

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	stdpath "path"
	"path/filepath"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/media"
	"github.com/OpenListTeam/OpenList/v4/internal/media/imgmime"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/gin-gonic/gin"
)

// PublicMediaProxy 代理外部封面图（TMDB/豆瓣/Discogs），解决客户端直连被墙的问题。
// 用法：/api/public/media/proxy?url=https%3A%2F%2Fimage.tmdb.org%2F...&sign=...（cover_url 自带签名，缺失/错误/过期为 403）
func PublicMediaProxy(c *gin.Context) {
	if !MediaEnabled() {
		c.Status(http.StatusNotFound)
		return
	}
	raw := c.Query("url")
	if !requireMediaSign(c, mediaProxySignPrefix, raw) {
		return
	}
	if raw == "" {
		c.Status(http.StatusBadRequest)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || media.SafeCoverURL(u) != nil {
		c.Status(http.StatusForbidden)
		return
	}
	resp, err := media.FetchCover(c.Request.Context(), u.String())
	if err != nil && setting.GetBool(conf.MediaCoverGateway) {
		// 直连失败时回退到公共图片网关（部分网络环境下 TMDB 图片域名不可达）。
		// 这会把封面 URL 交给第三方 wsrv.nl，因此默认关闭，需管理员显式开启。
		gateway := "https://wsrv.nl/?url=" + url.QueryEscape(strings.TrimPrefix(strings.TrimPrefix(u.String(), "https://"), "http://"))
		resp, err = media.FetchCover(c.Request.Context(), gateway)
	}
	if err != nil {
		utils.Log.Warnf("media cover proxy failed: %s: %v", u.String(), err)
		c.Status(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	// 不信任上游 Content-Type：读取有限字节后按魔数判定，只输出四种栅格图片，
	// 避免 image/svg+xml 等可执行脚本的类型以本站源被浏览器渲染。
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || len(data) == 0 {
		c.Status(http.StatusBadGateway)
		return
	}
	ct := imgmime.Sniff(data)
	if ct == "" {
		c.Status(http.StatusUnsupportedMediaType)
		return
	}
	setSafeImageHeaders(c)
	c.Header("Cache-Control", "public, max-age=604800")
	c.Data(http.StatusOK, ct, data)
}

// setSafeImageHeaders 为用户可影响内容的图片响应加上防 XSS 头。
func setSafeImageHeaders(c *gin.Context) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", `inline; filename="cover"`)
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
}

// PublicMediaThumb 读取本地缩略图目录中的文件。
// 缩略图由刮削器写入本地磁盘（非 VFS），因此不能用 /d 读取。
// 用法：/api/public/media/thumb?path=/imgs/xxx.jpg&sign=...（cover_url 自带签名，缺失/错误/过期为 403）
func PublicMediaThumb(c *gin.Context) {
	if !MediaEnabled() {
		c.Status(http.StatusNotFound)
		return
	}
	if !requireMediaSign(c, mediaThumbSignPrefix, stdpath.Clean("/"+c.Query("path"))) {
		return
	}
	if p := strings.TrimPrefix(stdpath.Clean("/"+c.Query("path")), "/"); p == "" || p == "." {
		c.Status(http.StatusBadRequest)
		return
	}
	full, err := media.ThumbFilePath(c.Query("path"))
	if errors.Is(err, media.ErrThumbOutside) {
		c.Status(http.StatusForbidden)
		return
	}
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	// 只按扩展名白名单输出栅格图片，拒绝 svg / html 等可执行类型
	var ct string
	switch strings.ToLower(filepath.Ext(full)) {
	case ".jpg", ".jpeg":
		ct = "image/jpeg"
	case ".png":
		ct = "image/png"
	case ".gif":
		ct = "image/gif"
	case ".webp":
		ct = "image/webp"
	default:
		c.Status(http.StatusUnsupportedMediaType)
		return
	}
	setSafeImageHeaders(c)
	c.Header("Content-Type", ct)
	c.Header("Cache-Control", "public, max-age=604800")
	c.File(full)
}
