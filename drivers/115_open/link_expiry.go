package _115_open

import (
	"net/url"
	"strconv"
	"time"
)

// 115 CDN download URLs carry `?t=<unix>` marking when the signed URL stops
// serving bytes. The real lifetime is often a few minutes, well below
// OpenList's default link-cache TTL — when the cache outlives the URL, OP
// hands out an expired CDN link and the response is "200 OK + empty body".
//
// parseCDNExpiry extracts that timestamp and turns it into a TTL suitable
// for model.Link.Expiration, with a safety margin so OP refreshes slightly
// before the CDN actually rejects.
const (
	cdnExpirySafetyMargin = 60 * time.Second
	cdnExpiryMinimum      = 1 * time.Second
)

func parseCDNExpiry(rawURL string) (time.Duration, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return 0, false
	}
	tStr := parsed.Query().Get("t")
	if tStr == "" {
		return 0, false
	}
	tUnix, err := strconv.ParseInt(tStr, 10, 64)
	if err != nil {
		return 0, false
	}
	remaining := time.Until(time.Unix(tUnix, 0)) - cdnExpirySafetyMargin
	if remaining < cdnExpiryMinimum {
		return cdnExpiryMinimum, true
	}
	return remaining, true
}
