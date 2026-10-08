package _115_open

import (
	"context"
	"sync"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	log "github.com/sirupsen/logrus"
)

const (
	// uploadTokenMargin is how long before the reported expiry a cached OSS
	// token stops being handed out.
	uploadTokenMargin = 5 * time.Minute
	// uploadTokenFallbackTTL applies when Expiration can't be parsed.
	uploadTokenFallbackTTL = 10 * time.Minute
)

// uploadTokenCache keeps the OSS token UploadGetToken returns. One call costs a
// rate-limit slot and the token is good for much longer than a small file's
// upload, so every file needn't fetch its own.
type uploadTokenCache struct {
	mu      sync.Mutex
	token   *sdk.UploadGetTokenResp
	expires time.Time
}

// uploadTokenExpiry turns the token's Expiration into the time to stop using it.
func uploadTokenExpiry(token *sdk.UploadGetTokenResp, now time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, token.Expiration); err == nil {
		return t.Add(-uploadTokenMargin)
	}
	return now.Add(uploadTokenFallbackTTL)
}

// get returns the cached token while it is fresh, otherwise fetches a new one.
func (c *uploadTokenCache) get(ctx context.Context, fetch func(context.Context) (*sdk.UploadGetTokenResp, error)) (*sdk.UploadGetTokenResp, error) {
	return c.refresh(ctx, nil, fetch)
}

// refresh replaces stale (a token the server rejected) with a new one. If
// another upload already replaced it, that newer token is returned instead.
func (c *uploadTokenCache) refresh(ctx context.Context, stale *sdk.UploadGetTokenResp, fetch func(context.Context) (*sdk.UploadGetTokenResp, error)) (*sdk.UploadGetTokenResp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != nil && c.token != stale && time.Now().Before(c.expires) {
		return c.token, nil
	}
	token, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	c.token, c.expires = token, uploadTokenExpiry(token, time.Now())
	log.Debugf("[115] upload token cached until %s", c.expires.Format(time.RFC3339))
	return token, nil
}

func (d *Open115) fetchUploadToken(ctx context.Context) (*sdk.UploadGetTokenResp, error) {
	if err := d.waitAPI(ctx, "GetToken"); err != nil {
		return nil, err
	}
	return d.client.UploadGetToken(ctx)
}

func (d *Open115) uploadToken(ctx context.Context) (*sdk.UploadGetTokenResp, error) {
	return d.tokens.get(ctx, d.fetchUploadToken)
}

func (d *Open115) refreshUploadToken(ctx context.Context, stale *sdk.UploadGetTokenResp) (*sdk.UploadGetTokenResp, error) {
	return d.tokens.refresh(ctx, stale, d.fetchUploadToken)
}
