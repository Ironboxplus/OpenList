package bootstrap

import (
	"context"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/stats"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"golang.org/x/time/rate"
)

type blockBurstLimiter struct {
	*rate.Limiter
}

func (l blockBurstLimiter) WaitN(ctx context.Context, total int) error {
	for total > 0 {
		n := l.Burst()
		if l.Limiter.Limit() == rate.Inf || n > total {
			n = total
		}
		err := l.Limiter.WaitN(ctx, n)
		if err != nil {
			return err
		}
		total -= n
	}
	return nil
}

func streamFilterNegative(limit int) (rate.Limit, int) {
	if limit < 0 {
		return rate.Inf, 0
	}
	return rate.Limit(limit) * 1024.0, limit * 1024
}

func initLimiter(limiter *stream.Limiter, s string, kind stats.Kind) {
	clientDownLimit, burst := streamFilterNegative(setting.GetInt(s, -1))
	// Counted here, once, for every byte that passes (see internal/stats).
	*limiter = stats.Wrap(kind, blockBurstLimiter{Limiter: rate.NewLimiter(clientDownLimit, burst)})
	op.RegisterSettingChangingCallback(func() {
		newLimit, newBurst := streamFilterNegative(setting.GetInt(s, -1))
		(*limiter).SetLimit(newLimit)
		(*limiter).SetBurst(newBurst)
	})
}

func InitStreamLimit() {
	initLimiter(&stream.ClientDownloadLimit, conf.StreamMaxClientDownloadSpeed, stats.ClientDownload)
	initLimiter(&stream.ClientUploadLimit, conf.StreamMaxClientUploadSpeed, stats.ClientUpload)
	initLimiter(&stream.ServerDownloadLimit, conf.StreamMaxServerDownloadSpeed, stats.ServerDownload)
	initLimiter(&stream.ServerUploadLimit, conf.StreamMaxServerUploadSpeed, stats.ServerUpload)
}

// InitStats starts the traffic counters' sampling and saving. The database
// must be up; Release saves what is still pending.
func InitStats() { stats.Start(stats.DBStore{}) }
