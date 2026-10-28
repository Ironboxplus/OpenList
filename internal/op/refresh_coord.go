package op

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// RefreshReport is one token-refresh attempt (or gated skip) as seen by a
// driver. It never carries token values; the hashes are short prefixes.
type RefreshReport struct {
	Mount             string
	Time              time.Time
	Trigger           string // API error code, "proactive", "manual", "refresh-request"
	Outcome           string // ok | api_error | transport_error | skipped_dead | skipped_cooldown | skipped_policy
	ErrCode           int64
	GateDead          bool
	CooldownRemaining time.Duration
	ExpiresIn         int64 // seconds, ok only
	Duration          time.Duration
	OldRefreshHash    string
	NewRefreshHash    string
	// AccessTokenHash identifies the access token obtained (ok only), so a
	// coordinator can attach the lifetime to the pair that later gets shared.
	AccessTokenHash string
}

// RefreshCoordinator lets a cluster layer decide which node may spend a
// single-use refresh_token, and observe every refresh. Drivers consult it; they
// never import the cluster package.
type RefreshCoordinator interface {
	// MayRefresh reports whether this node may call the provider's refresh
	// endpoint automatically for the mount. False for non-refresher members.
	MayRefresh(mount string) bool
	// ReportRefresh observes a refresh attempt or gated skip.
	ReportRefresh(r RefreshReport)
	// RefreshRejected is called when a request was rejected with an access-token
	// error and this node was not allowed to refresh: the coordinator should
	// obtain a fresh pair from the refresher.
	RefreshRejected(mount string, accessTokenHash string)
}

// TokenRefresher is implemented by drivers whose token can be refreshed ahead of
// expiry by the group's refresher.
type TokenRefresher interface {
	// RefreshTokenNow spends the refresh_token now and proves the new pair with
	// an authenticated request, so the usual token-valid publication follows.
	RefreshTokenNow(ctx context.Context, trigger string) error
}

var (
	refreshCoordMu sync.RWMutex
	refreshCoord   RefreshCoordinator
)

// SetRefreshCoordinator installs (or clears, with nil) the process-wide coordinator.
func SetRefreshCoordinator(c RefreshCoordinator) {
	refreshCoordMu.Lock()
	refreshCoord = c
	refreshCoordMu.Unlock()
}

func currentRefreshCoordinator() RefreshCoordinator {
	refreshCoordMu.RLock()
	defer refreshCoordMu.RUnlock()
	return refreshCoord
}

// RefreshAllowed reports whether the node may auto-refresh the mount's token.
// Without a coordinator (cluster off) every node refreshes for itself.
func RefreshAllowed(mount string) bool {
	if c := currentRefreshCoordinator(); c != nil {
		return c.MayRefresh(mount)
	}
	return true
}

// ReportRefresh forwards a refresh observation to the coordinator, if any.
func ReportRefresh(r RefreshReport) {
	if c := currentRefreshCoordinator(); c != nil {
		c.ReportRefresh(r)
	}
}

// NotifyRefreshRejected tells the coordinator a non-refresher's request was
// rejected for an invalid access token.
func NotifyRefreshRejected(mount, accessTokenHash string) {
	if c := currentRefreshCoordinator(); c != nil {
		c.RefreshRejected(mount, accessTokenHash)
	}
}

// TokenHash is the short, non-reversible identifier used to correlate a token
// across log lines and nodes without ever writing the token itself.
func TokenHash(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:8]
}
