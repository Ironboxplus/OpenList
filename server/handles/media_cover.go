package handles

import (
	"net/http"
	"net/url"
	stdpath "path"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/gin-gonic/gin"
)

// The cover endpoints under /api/public/media are reachable without a login, so
// they only serve what a response handed out: every cover_url carries a
// signature of the path (thumb) or the source URL (proxy). The data is prefixed
// by endpoint so a signature of one cannot be used on the other, and it expires
// with the other links (link_expiration).
const (
	mediaThumbSignPrefix = "media-thumb:"
	mediaProxySignPrefix = "media-proxy:"
)

// requireMediaSign answers 403 and returns false unless the request's sign
// query is a valid, unexpired signature of prefix+data.
func requireMediaSign(c *gin.Context, prefix, data string) bool {
	s := c.Query("sign")
	if s == "" || sign.Verify(prefix+data, s) != nil {
		c.Status(http.StatusForbidden)
		return false
	}
	return true
}

// mediaCoverURL turns a stored cover reference into the URL a client fetches.
//
//	data:...        base64 mode: the data itself
//	thumb:/imgs/x   runtime mode: the signed public thumb endpoint
//	http(s)://...   a remote cover: the signed proxy endpoint when media_proxy_cover
//	                is on, else the URL itself
//	/d/...          local (VFS) mode: the /d link with a fresh signature, like a
//	                file list (the stored one may have expired)
//
// Anything else (and empty) is returned as is.
func mediaCoverURL(ref string) string {
	switch {
	case ref == "", strings.HasPrefix(ref, "data:"):
		return ref
	case strings.HasPrefix(ref, "thumb:"):
		p := stdpath.Clean("/" + strings.TrimPrefix(ref, "thumb:"))
		return "/api/public/media/thumb?path=" + url.QueryEscape(p) + "&sign=" + url.QueryEscape(sign.Sign(mediaThumbSignPrefix+p))
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		if !setting.GetBool(conf.MediaProxyCover) {
			return ref
		}
		return "/api/public/media/proxy?url=" + url.QueryEscape(ref) + "&sign=" + url.QueryEscape(sign.Sign(mediaProxySignPrefix+ref))
	case strings.HasPrefix(ref, "/d/"):
		p := strings.TrimPrefix(ref, "/d")
		if i := strings.IndexByte(p, '?'); i >= 0 {
			p = p[:i]
		}
		return "/d" + (&url.URL{Path: p}).EscapedPath() + "?sign=" + url.QueryEscape(sign.Sign(p))
	}
	return ref
}

// MediaItemView is an item as the public APIs return it: the stored fields
// plus the URLs to fetch its images from.
type MediaItemView struct {
	model.MediaItem
	CoverURL    string `json:"cover_url,omitempty"`
	BackdropURL string `json:"backdrop_url,omitempty"`
}

func mediaItemView(it *model.MediaItem) MediaItemView {
	return MediaItemView{MediaItem: *it, CoverURL: mediaCoverURL(it.Cover), BackdropURL: mediaCoverURL(it.Backdrop)}
}

func mediaItemViews(items []model.MediaItem) []MediaItemView {
	out := make([]MediaItemView, len(items))
	for i := range items {
		out[i] = mediaItemView(&items[i])
	}
	return out
}

// MediaAlbumView is an album with the URL of its cover.
type MediaAlbumView struct {
	db.AlbumInfo
	CoverURL string `json:"cover_url,omitempty"`
}

func mediaAlbumViews(albums []db.AlbumInfo) []MediaAlbumView {
	out := make([]MediaAlbumView, len(albums))
	for i := range albums {
		out[i] = MediaAlbumView{AlbumInfo: albums[i], CoverURL: mediaCoverURL(albums[i].Cover)}
	}
	return out
}
