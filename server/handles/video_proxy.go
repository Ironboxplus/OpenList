package handles

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// videoProxyPath is the route (relative to conf.URL.Path, sibling to /p) that
// streams an arbitrary signed upstream media URL through OpenList. It exists so
// the browser never fetches a provider's transcoded CDN directly (which fails
// CORS, e.g. 115's online-play HLS); everything is fetched same-origin and the
// provider request happens server-side.
const videoProxyPath = "/video_proxy"

// BuildVideoProxyURL wraps an upstream media URL in a signed OpenList proxy URL.
// The signature (over the raw upstream URL) prevents the endpoint from acting as
// an open relay: only URLs OpenList itself produced are accepted.
func BuildVideoProxyURL(apiURL, rawURL string) string {
	return apiURL + videoProxyPath +
		"?url=" + url.QueryEscape(rawURL) +
		"&sign=" + url.QueryEscape(sign.Sign(rawURL))
}

// isM3U8 reports whether the upstream response is an HLS manifest, by
// content-type or by the upstream URL's path extension.
func isM3U8(contentType, rawURL string) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "mpegurl") {
		return true
	}
	if u, err := url.Parse(rawURL); err == nil {
		if strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") {
			return true
		}
	}
	return false
}

// rewriteM3U8 rewrites every segment / sub-playlist / key URI in an HLS manifest
// so they are also fetched through the OpenList video proxy. baseRawURL is the
// upstream URL the manifest was fetched from (used to resolve relative URIs);
// apiURL is the OpenList base used to build the proxy links. Without this, the
// segment URLs inside the manifest would still point at the provider CDN and
// re-introduce the CORS failure we are trying to avoid.
func rewriteM3U8(manifest, baseRawURL, apiURL string) string {
	baseURL, _ := url.Parse(baseRawURL)
	var b strings.Builder
	lines := strings.Split(manifest, "\n")
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			b.WriteString(line)
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			// Tag line: rewrite any URI="..." attribute (EXT-X-KEY, EXT-X-MAP,
			// EXT-X-MEDIA, EXT-X-I-FRAME-STREAM-INF, EXT-X-SESSION-KEY, ...).
			b.WriteString(rewriteTagURIs(line, baseURL, apiURL))
			continue
		}
		// Resource line: a segment or sub-playlist URI on its own line.
		b.WriteString(proxyResolve(trimmed, baseURL, apiURL))
	}
	return b.String()
}

// rewriteTagURIs rewrites every URI="..." occurrence on an HLS tag line.
func rewriteTagURIs(line string, baseURL *url.URL, apiURL string) string {
	const marker = `URI="`
	var b strings.Builder
	rest := line
	for {
		idx := strings.Index(rest, marker)
		if idx < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:idx+len(marker)])
		rest = rest[idx+len(marker):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(proxyResolve(rest[:end], baseURL, apiURL))
		b.WriteByte('"')
		rest = rest[end+1:]
	}
	return b.String()
}

// proxyResolve resolves ref against the manifest's base URL (no-op if ref is
// already absolute) and wraps the result in a signed proxy URL.
func proxyResolve(ref string, baseURL *url.URL, apiURL string) string {
	abs := ref
	if baseURL != nil {
		if u, err := url.Parse(ref); err == nil {
			abs = baseURL.ResolveReference(u).String()
		}
	}
	return BuildVideoProxyURL(apiURL, abs)
}

// hopByHopOrRangeHeaders are upstream response headers worth forwarding to the
// client for non-manifest passthrough (segments / progressive mp4).
var passthroughHeaders = []string{
	"Content-Type",
	"Content-Length",
	"Content-Range",
	"Accept-Ranges",
	"Last-Modified",
	"ETag",
}

// VideoProxy streams a signed upstream media URL through OpenList. For HLS
// manifests it rewrites the inner URIs to route back through this same endpoint;
// everything else is streamed verbatim (with Range support for seeking).
func VideoProxy(c *gin.Context) {
	rawURL := c.Query("url")
	signParam := c.Query("sign")
	if rawURL == "" {
		common.ErrorStrResp(c, "missing url", 400)
		return
	}
	if err := sign.Verify(rawURL, signParam); err != nil {
		common.ErrorResp(c, err, 401)
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, rawURL, nil)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	// Forward Range so the player can byte-range seek within segments / mp4.
	if rng := c.GetHeader("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	req.Header.Set("User-Agent", base.UserAgent)

	resp, err := base.HttpClient.Do(req)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	defer resp.Body.Close()

	if isM3U8(resp.Header.Get("Content-Type"), rawURL) {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		out := rewriteM3U8(string(body), rawURL, common.GetApiUrl(c))
		c.Header("Content-Type", "application/vnd.apple.mpegurl")
		c.Header("Cache-Control", "no-cache")
		c.String(resp.StatusCode, out)
		return
	}

	for _, h := range passthroughHeaders {
		if v := resp.Header.Get(h); v != "" {
			c.Header(h, v)
		}
	}
	c.Status(resp.StatusCode)
	if c.Request.Method != http.MethodHead {
		_, _ = io.Copy(c.Writer, resp.Body)
	}
}
