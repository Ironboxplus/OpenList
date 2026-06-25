package common

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"maps"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/net"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

func Proxy(w http.ResponseWriter, r *http.Request, link *model.Link, file model.Obj) error {
	// if link.MFile != nil {
	// 	attachHeader(w, file, link)
	// 	http.ServeContent(w, r, file.GetName(), file.ModTime(), link.MFile)
	// 	return nil
	// }

	if link.Concurrency > 0 || link.PartSize > 0 {
		attachHeader(w, file, link)
		size := link.ContentLength
		if size <= 0 {
			size = file.GetSize()
		}
		rrf, _ := stream.GetRangeReaderFromLink(size, link)
		if link.RangeReader == nil {
			r = r.WithContext(context.WithValue(r.Context(), conf.RequestHeaderKey, r.Header))
		}
		return net.ServeHTTP(w, r, file.GetName(), file.ModTime(), size, &model.RangeReadCloser{
			RangeReader: rrf,
		})
	}

	if link.RangeReader != nil {
		attachHeader(w, file, link)
		size := link.ContentLength
		if size <= 0 {
			size = file.GetSize()
		}
		return net.ServeHTTP(w, r, file.GetName(), file.ModTime(), size, &model.RangeReadCloser{
			RangeReader: link.RangeReader,
		})
	}

	//transparent proxy
	header := net.ProcessHeader(r.Header, link.Header)
	res, err := net.RequestHttp(r.Context(), r.Method, header, link.URL)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	maps.Copy(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)
	if r.Method == http.MethodHead {
		return nil
	}
	_, err = utils.CopyWithBuffer(w, &stream.RateLimitReader{
		Reader:  res.Body,
		Limiter: stream.ServerDownloadLimit,
		Ctx:     r.Context(),
	})
	return err
}
// ApplyProxyUserAgent overrides the User-Agent on the client request header used to
// resolve and proxy a download when the storage configured a custom one. It mutates
// the (request-scoped) request header BEFORE the link is resolved — deliberately not
// the cached *model.Link — so it:
//   - feeds drivers that sign the download URL with the request UA (e.g. 115 calls
//     DownURL with args.Header's User-Agent), and is keyed correctly by drivers whose
//     LinkCacheMode includes the UA;
//   - covers transparent-proxy drivers too, since ProcessHeader copies the request
//     header (the override) when link.Header carries no User-Agent;
//   - never races on or pollutes the shared link cache (the previous approach mutated
//     link.Header after caching, leaking the UA across concurrent/later downloads).
//
// Empty config keeps the default behavior (passthrough the client's User-Agent). A
// driver that pins a fixed UA of its own still wins, which is intended — that UA is
// usually required for the link to resolve.
func ApplyProxyUserAgent(r *http.Request, storage *model.Storage) {
	if r == nil || storage == nil || storage.ProxyUserAgent == "" {
		return
	}
	r.Header.Set("User-Agent", storage.ProxyUserAgent)
}

func attachHeader(w http.ResponseWriter, file model.Obj, link *model.Link) {
	fileName := file.GetName()
	w.Header().Set("Content-Disposition", utils.GenerateContentDisposition(fileName))
	w.Header().Set("Content-Type", utils.GetMimeType(fileName))
	size := link.ContentLength
	if size <= 0 {
		size = file.GetSize()
	}
	w.Header().Set("Etag", GetEtag(file, size))
	contentType := link.Header.Get("Content-Type")
	if len(contentType) > 0 {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", utils.GetMimeType(fileName))
	}
}
func GetEtag(file model.Obj, size int64) string {
	hash := ""
	for _, v := range file.GetHash().Export() {
		if v > hash {
			hash = v
		}
	}
	if len(hash) > 0 {
		return fmt.Sprintf(`"%s"`, hash)
	}
	// 参考nginx
	return fmt.Sprintf(`"%x-%x"`, file.ModTime().Unix(), size)
}

func ProxyRange(ctx context.Context, link *model.Link, size int64) *model.Link {
	if link.RangeReader == nil && !strings.HasPrefix(link.URL, GetApiUrl(ctx)+"/") {
		if link.ContentLength > 0 {
			size = link.ContentLength
		}
		rrf, err := stream.GetRangeReaderFromLink(size, link)
		if err == nil {
			return &model.Link{
				RangeReader:   rrf,
				ContentLength: size,
			}
		}
	}
	return link
}

type InterceptResponseWriter struct {
	http.ResponseWriter
	io.Writer
}

func (iw *InterceptResponseWriter) Write(p []byte) (int, error) {
	return iw.Writer.Write(p)
}

type WrittenResponseWriter struct {
	http.ResponseWriter
	written bool
}

func (ww *WrittenResponseWriter) Write(p []byte) (int, error) {
	n, err := ww.ResponseWriter.Write(p)
	if !ww.written && n > 0 {
		ww.written = true
	}
	return n, err
}

func (ww *WrittenResponseWriter) IsWritten() bool {
	return ww.written
}

func GenerateDownProxyURL(storage *model.Storage, reqPath string) string {
	if storage.DownProxyURL == "" {
		return ""
	}
	query := ""
	if !storage.DisableProxySign {
		query = "?sign=" + sign.Sign(reqPath)
	}
	return fmt.Sprintf("%s%s%s",
		strings.Split(storage.DownProxyURL, "\n")[0],
		utils.EncodePath(reqPath, true),
		query,
	)
}
