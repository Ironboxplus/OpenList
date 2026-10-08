package _115_open

import (
	"github.com/OpenListTeam/OpenList/v4/internal/lane"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// apiStatsInterval is how often the call counts are logged.
const apiStatsInterval = 60 * time.Second

// apiStats counts API calls (each one a rate-limit slot) by method, across all
// 115 mounts, and how long callers waited for the limiter.
var apiStats struct {
	once   sync.Once
	counts sync.Map // method name -> *atomic.Int64
	// Time callers spent waiting for the limiter, by lane: foreground is the
	// UI and API handlers, background is sync jobs (see internal/lane).
	fgWaited atomic.Int64
	bgWaited atomic.Int64
}

// waitAPI is WaitLimit for one named API call: it counts the call and the
// time spent waiting.
func (d *Open115) waitAPI(ctx context.Context, method string) error {
	apiStats.once.Do(func() { go logAPIStats(apiStatsInterval) })
	c, ok := apiStats.counts.Load(method)
	if !ok {
		c, _ = apiStats.counts.LoadOrStore(method, new(atomic.Int64))
	}
	c.(*atomic.Int64).Add(1)
	start := time.Now()
	err := d.WaitLimit(ctx)
	if lane.IsBackground(ctx) {
		apiStats.bgWaited.Add(int64(time.Since(start)))
	} else {
		apiStats.fgWaited.Add(int64(time.Since(start)))
	}
	return err
}

// takeAPIStats returns the counts and wait time since the last call and resets them.
func takeAPIStats() (counts map[string]int64, fgWaited, bgWaited time.Duration) {
	counts = map[string]int64{}
	apiStats.counts.Range(func(k, v any) bool {
		if n := v.(*atomic.Int64).Swap(0); n > 0 {
			counts[k.(string)] = n
		}
		return true
	})
	return counts, time.Duration(apiStats.fgWaited.Swap(0)), time.Duration(apiStats.bgWaited.Swap(0))
}

// formatAPIStats renders one log line; the methods that dominate upload cost
// are always shown, any other only when called.
func formatAPIStats(counts map[string]int64, fgWaited, bgWaited time.Duration) string {
	var total int64
	for _, n := range counts {
		total += n
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "api %ds: total=%d UploadInit=%d Get=%d List=%d GetToken=%d",
		int(apiStatsInterval/time.Second), total, counts["UploadInit"], counts["Get"], counts["List"], counts["GetToken"])
	var others []string
	for name, n := range counts {
		switch name {
		case "UploadInit", "Get", "List", "GetToken":
		default:
			others = append(others, fmt.Sprintf("%s=%d", name, n))
		}
	}
	sort.Strings(others)
	for _, o := range others {
		sb.WriteString(" " + o)
	}
	fmt.Fprintf(&sb, " waited=%.1fs fg_waited=%.1fs bg_waited=%.1fs",
		(fgWaited + bgWaited).Seconds(), fgWaited.Seconds(), bgWaited.Seconds())
	return sb.String()
}

func logAPIStats(every time.Duration) {
	for range time.Tick(every) {
		counts, fgWaited, bgWaited := takeAPIStats()
		if len(counts) == 0 {
			continue
		}
		log.Infof("[115] %s", formatAPIStats(counts, fgWaited, bgWaited))
	}
}
