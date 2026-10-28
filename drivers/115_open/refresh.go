package _115_open

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	log "github.com/sirupsen/logrus"
)

// skipLogEvery bounds the log volume of gated refresh skips: a player retrying
// at 1 req/s on a non-refresher member must not flood the log.
const skipLogEvery = 30 * time.Second

// refreshState is the per-driver bookkeeping of refresh observation.
type refreshState struct {
	// policySkipToken is the access token whose rejection was answered by the
	// refresh policy (we may not refresh). The access-token-invalid callback that
	// follows must not be mistaken for a dead credential chain.
	policySkipToken atomic.Pointer[string]
	lastSkipLog     atomic.Int64
}

// onRefreshResult logs every refresh attempt and gated skip (never a token) and
// forwards it to the cluster coordinator.
func (d *Open115) onRefreshResult(mount string, ev sdk.RefreshEvent) {
	rep := op.RefreshReport{
		Mount:             mount,
		Time:              time.Now(),
		Trigger:           ev.Trigger,
		Outcome:           ev.Outcome,
		ErrCode:           ev.ErrCode,
		GateDead:          ev.GateDead,
		CooldownRemaining: ev.CooldownRemaining,
		ExpiresIn:         ev.ExpiresIn,
		Duration:          ev.Duration,
		OldRefreshHash:    ev.OldRefreshHash,
		NewRefreshHash:    ev.NewRefreshHash,
	}
	if ev.Outcome == sdk.RefreshOK {
		rep.AccessTokenHash = op.TokenHash(d.Addition.AccessToken)
	}
	if ev.Outcome != sdk.RefreshSkippedPolicy {
		d.refresh.policySkipToken.Store(nil)
	}
	switch ev.Outcome {
	case sdk.RefreshSkippedPolicy:
		tok := d.Addition.AccessToken
		d.refresh.policySkipToken.Store(&tok)
		d.logSkip("refresh skipped (not the group refresher)", mount, ev)
		op.ReportRefresh(rep)
		op.NotifyRefreshRejected(mount, op.TokenHash(tok))
		return
	case sdk.RefreshSkippedDead, sdk.RefreshSkippedCooldown:
		d.logSkip("refresh skipped (gate)", mount, ev)
	case sdk.RefreshOK:
		log.Infof("[115_open] refresh ok mount=%s trigger=%s gate_dead=%v cooldown=%s expires_in=%ds took=%s old=%s new=%s",
			mount, ev.Trigger, ev.GateDead, ev.CooldownRemaining.Round(time.Second), ev.ExpiresIn,
			ev.Duration.Round(time.Millisecond), ev.OldRefreshHash, ev.NewRefreshHash)
	default:
		log.Warnf("[115_open] refresh FAILED mount=%s trigger=%s outcome=%s code=%d gate_dead=%v cooldown=%s took=%s old=%s err=%v",
			mount, ev.Trigger, ev.Outcome, ev.ErrCode, ev.GateDead, ev.CooldownRemaining.Round(time.Second),
			ev.Duration.Round(time.Millisecond), ev.OldRefreshHash, ev.Err)
	}
	op.ReportRefresh(rep)
}

func (d *Open115) logSkip(msg, mount string, ev sdk.RefreshEvent) {
	now := time.Now().UnixNano()
	last := d.refresh.lastSkipLog.Load()
	if now-last < int64(skipLogEvery) || !d.refresh.lastSkipLog.CompareAndSwap(last, now) {
		return
	}
	log.Infof("[115_open] %s mount=%s trigger=%s outcome=%s gate_dead=%v cooldown=%s",
		msg, mount, ev.Trigger, ev.Outcome, ev.GateDead, ev.CooldownRemaining.Round(time.Second))
}

// policySkipped reports whether the rejection of accessToken was only answered
// by the refresh policy: the token expired, but the chain is not known dead.
func (d *Open115) policySkipped(accessToken string) bool {
	p := d.refresh.policySkipToken.Load()
	return p != nil && *p == accessToken
}

// RefreshTokenNow implements op.TokenRefresher. It spends the refresh_token
// regardless of the cluster policy (the caller is the group's refresher), then
// proves the new pair with an authenticated request so the normal token-valid
// publication to the cluster follows.
func (d *Open115) RefreshTokenNow(ctx context.Context, trigger string) error {
	if d.client == nil {
		return errors.New("115 client not initialised")
	}
	if _, err := d.client.RefreshWithTrigger(ctx, trigger); err != nil {
		var apiErr *sdk.Error
		if errors.As(err, &apiErr) && apiErr.Code != sdk.CodeRefreshFrequently && sdk.Is401Started(apiErr.Code) {
			d.handleAccessTokenInvalid(d.Addition.AccessToken)
		}
		return err
	}
	if err := d.waitAPI(ctx, "UserInfo"); err != nil {
		return err
	}
	_, err := d.client.UserInfo(ctx)
	return err
}

var _ op.TokenRefresher = (*Open115)(nil)
