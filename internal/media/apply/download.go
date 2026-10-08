package apply

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
)

// MaxImageBytes caps one downloaded image.
const MaxImageBytes = 10 << 20

// DownloadOpts configures NewDownloader.
type DownloadOpts struct {
	Proxy        string        // explicit outbound proxy ("" = environment)
	Timeout      time.Duration // default 20s
	AllowPrivate bool          // allow loopback / private / link-local targets (tests)
}

// NewDownloader returns a Downloader that fetches http(s) images only,
// follows at most 5 redirects, refuses oversized bodies, and — when no proxy
// is in play — refuses to connect to loopback, private and link-local
// addresses (checked on the address actually dialed, so DNS rebinding does
// not help). With a proxy the proxy does the resolving and the check is moot.
func NewDownloader(o DownloadOpts) (Downloader, error) {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second}
	usingProxy := false
	if o.Proxy != "" {
		pu, err := url.Parse(o.Proxy)
		if err != nil || pu.Host == "" {
			return nil, errors.New("invalid proxy URL")
		}
		tr.Proxy = http.ProxyURL(pu)
		usingProxy = true
	} else {
		tr.Proxy = http.ProxyFromEnvironment
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			if os.Getenv(k) != "" {
				usingProxy = true
			}
		}
	}
	if !o.AllowPrivate && !usingProxy {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
				return errors.New("refusing to connect to a private or loopback address")
			}
			return nil
		}
	}
	tr.DialContext = dialer.DialContext
	hc := &http.Client{
		Transport: tr, Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to a non-http scheme")
			}
			return nil
		},
	}
	return func(ctx context.Context, rawURL string) ([]byte, error) {
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("only http(s) image URLs are allowed")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "openlist-media/"+scraper.Version)
		req.Header.Set("Accept", "image/*")
		resp, err := hc.Do(req)
		if err != nil {
			var ue *url.Error
			cause := err
			if errors.As(err, &ue) {
				cause = ue.Err
			}
			class := scraper.ClassOf(cause)
			if class == scraper.ClassOther {
				class = ""
			}
			return nil, &scraper.Error{Provider: "image", Class: orOther(class), Msg: scraper.MaskSecrets("download failed: " + cause.Error()), Err: cause}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, &scraper.Error{Provider: "image", Class: scraper.ClassOther, Status: resp.StatusCode, Msg: "download failed"}
		}
		if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "application/octet-stream") {
			return nil, fmt.Errorf("not an image (Content-Type %s)", ct)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, MaxImageBytes+1))
		if err != nil {
			return nil, &scraper.Error{Provider: "image", Class: scraper.ClassNetwork, Msg: "download failed: " + err.Error(), Err: err}
		}
		if len(data) > MaxImageBytes {
			return nil, fmt.Errorf("image is larger than %d bytes", MaxImageBytes)
		}
		return data, nil
	}, nil
}

func orOther(c string) string {
	if c == "" {
		return scraper.ClassOther
	}
	return c
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}
