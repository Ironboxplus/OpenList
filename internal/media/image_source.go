package media

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	stdpath "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/media/imgmime"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
)

// MaxImageSize is how many bytes of one image are read.
const MaxImageSize = 8 << 20

// 允许代理的封面来源域名（TMDB / 豆瓣 / Discogs）。
// 仅代理图片，避免该接口被当作通用反向代理滥用。
var coverAllowedHosts = []string{
	"image.tmdb.org",
	"tmdb.org",
	"doubanio.com",
	"douban.com",
	"discogs.com",
}

func hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".local") {
		return false
	}
	// 显式拒绝 IP 字面量，避免白名单域名被解析/重定向到内网
	if ip := net.ParseIP(host); ip != nil {
		return false
	}
	for _, h := range coverAllowedHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// SafeCoverURL 校验封面地址的协议与域名（含重定向后的每一跳）
func SafeCoverURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("empty url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme")
	}
	if !hostAllowed(u.Hostname()) {
		return fmt.Errorf("host not allowed")
	}
	return nil
}

// FetchCover 拉取图片，仅接受 200 且 Content-Type 为 image/*
// 重定向的每一跳都重新做域名校验，避免白名单域名 302 到内网造成 SSRF。
func FetchCover(ctx context.Context, raw string) (*http.Response, error) {
	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			return SafeCoverURL(req.URL)
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; OpenList-Media/1.0)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return resp, nil
}

// ThumbnailRoot 返回运行目录模式下缩略图的本地根目录（<data>/<thumbnail_path>）
func ThumbnailRoot() string {
	dir := setting.GetStr(conf.MediaThumbnailPath, "imgs")
	return scraper.RuntimeThumbRoot(dir)
}

// ErrThumbOutside is a thumb path that leaves the thumbnail root.
var ErrThumbOutside = errors.New("outside the thumbnail directory")

// ThumbFilePath maps the path of a "thumb:" reference (as "/imgs/x.jpg") to the
// file under ThumbnailRoot. The path is cleaned; one that leaves the root, or
// that is not under the thumbnail directory, is an error.
func ThumbFilePath(p string) (string, error) {
	reqPath := strings.TrimPrefix(stdpath.Clean("/"+p), "/")
	// stdpath.Clean 已消解 ../ 段；这里不能再用 Contains("..") 粗暴过滤，会误杀 a..b.jpg 这类合法文件名。
	// 越界由下方 filepath.Rel 校验兜底。
	if reqPath == "" || reqPath == "." {
		return "", errors.New("empty thumb path")
	}
	base := strings.Trim(setting.GetStr(conf.MediaThumbnailPath, "/imgs"), "/")
	if base != "" {
		if !strings.HasPrefix(reqPath, base+"/") {
			return "", errors.New("not in the thumbnail directory")
		}
		reqPath = strings.TrimPrefix(reqPath, base+"/")
	}
	root := ThumbnailRoot()
	full := filepath.Join(root, filepath.FromSlash(reqPath))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrThumbOutside
	}
	return full, nil
}

// LoadImage reads the image a stored cover reference (MediaItem.Cover, Backdrop,
// MediaEpisode.Still) points at, from this machine or the library:
//
//	data:...;base64,...  the data itself
//	thumb:/imgs/x.jpg    a file under the runtime thumbnail directory
//	http(s)://...        a remote cover (hosts as the cover proxy allows)
//	/d/...?sign=...      a VFS file (the query is dropped)
//	/any/vfs/path        a VFS file
//
// The type comes from the bytes: only jpeg, png, gif and webp are returned.
func LoadImage(ctx context.Context, ref string) ([]byte, string, error) {
	var data []byte
	var err error
	switch {
	case ref == "":
		return nil, "", errors.New("no image")
	case strings.HasPrefix(ref, "data:"):
		i := strings.Index(ref, ",")
		if i < 0 || !strings.HasSuffix(ref[:i], ";base64") {
			return nil, "", errors.New("unsupported data uri")
		}
		data, err = base64.StdEncoding.DecodeString(ref[i+1:])
	case strings.HasPrefix(ref, "thumb:"):
		var full string
		if full, err = ThumbFilePath(strings.TrimPrefix(ref, "thumb:")); err == nil {
			data, err = readFileLimited(full)
		}
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		u, perr := url.Parse(ref)
		if perr != nil {
			return nil, "", perr
		}
		if err = SafeCoverURL(u); err != nil {
			return nil, "", err
		}
		var resp *http.Response
		if resp, err = FetchCover(ctx, u.String()); err == nil {
			data, err = io.ReadAll(io.LimitReader(resp.Body, MaxImageSize))
			_ = resp.Body.Close()
		}
	case strings.HasPrefix(ref, "/"):
		p := ref
		if strings.HasPrefix(p, "/d/") {
			p = strings.TrimPrefix(p, "/d")
			if i := strings.IndexByte(p, '?'); i >= 0 {
				p = p[:i]
			}
		}
		r := FetchFileReader(ctx, stdpath.Clean(p))
		if r == nil {
			return nil, "", errors.New("cannot open " + p)
		}
		data, err = io.ReadAll(io.LimitReader(r, MaxImageSize))
		_ = r.Close()
	default:
		return nil, "", errors.New("unsupported image reference")
	}
	if err != nil {
		return nil, "", err
	}
	// 不信任来源的 Content-Type：按魔数判定，只输出四种栅格图片
	ct := imgmime.Sniff(data)
	if ct == "" {
		return nil, "", errors.New("not a supported image")
	}
	return data, ct, nil
}

func readFileLimited(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, MaxImageSize))
}
