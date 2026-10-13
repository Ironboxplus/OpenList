// Package stats counts the bytes this node relays. The four global stream
// limiters (internal/stream/limit.go) are the one place every relayed byte
// passes, so counting there needs no change at any driver or call site: Wrap
// decorates a limiter, and WaitN adds to a counter before it delegates.
//
// What is counted (and what is not):
//   - 302 redirects let the client talk to the storage directly: not counted.
//   - A rapid upload (hash match) moves no bytes: nothing to count.
//   - A retried read happens again and is counted again, as it was really sent.
//   - server_* is the traffic between this node and the storages (proxy
//     playback, /d proxy, WebDAV/S3 proxy reads, sync downloads, uploads
//     through drivers that honour the upload limiter); client_* is what FTP
//     (and the limited HTTP/WebDAV download/upload handlers) move to and from
//     clients. A proxied download is in both, so the two groups are shown side
//     by side and never added together.
//   - Only WaitN counts; the other Limiter methods (Wait, AllowN, ReserveN) are
//     forwarded untouched. No caller uses them on these four limiters today.
//   - A read that returns data together with an error (io.EOF) skips the
//     limiter in stream.RateLimitReader, so that last chunk is not counted.
//   - Cluster nodes count only themselves.
//
// Days are the server's local calendar days. A flush records its delta on the
// day it happens, whole (a delta that spans midnight is not split).
package stats

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	log "github.com/sirupsen/logrus"
)

type Kind string

const (
	ServerDownload Kind = "server_download"
	ServerUpload   Kind = "server_upload"
	ClientDownload Kind = "client_download"
	ClientUpload   Kind = "client_upload"
)

var kinds = [...]Kind{ServerDownload, ServerUpload, ClientDownload, ClientUpload}

func index(k Kind) int {
	for i, kk := range kinds {
		if kk == k {
			return i
		}
	}
	return -1
}

// Counts is one number per kind; its JSON keys are the kind names.
type Counts struct {
	ServerDownload int64 `json:"server_download"`
	ServerUpload   int64 `json:"server_upload"`
	ClientDownload int64 `json:"client_download"`
	ClientUpload   int64 `json:"client_upload"`
}

func (c *Counts) set(i int, v int64) {
	switch i {
	case 0:
		c.ServerDownload = v
	case 1:
		c.ServerUpload = v
	case 2:
		c.ClientDownload = v
	case 3:
		c.ClientUpload = v
	}
}

func (c Counts) get(i int) int64 {
	return [...]int64{c.ServerDownload, c.ServerUpload, c.ClientDownload, c.ClientUpload}[i]
}

func (c Counts) plus(o Counts) Counts {
	for i := range kinds {
		c.set(i, c.get(i)+o.get(i))
	}
	return c
}

// byKind builds Counts from a kind-name keyed map (the shape the store returns).
func byKind(m map[string]int64) Counts {
	var c Counts
	for i, k := range kinds {
		c.set(i, m[string(k)])
	}
	return c
}

// counter is the node-wide state. total only grows (the sampler differences
// it); pending is what the database has not seen yet.
type counter struct {
	total   [len(kinds)]atomic.Int64
	pending [len(kinds)]atomic.Int64
}

var global counter

func add(k Kind, n int) {
	i := index(k)
	if i < 0 || n <= 0 {
		return
	}
	global.total[i].Add(int64(n))
	global.pending[i].Add(int64(n))
}

// counting is a Limiter that counts what passes WaitN.
type counting struct {
	stream.Limiter
	kind Kind
}

func (c *counting) WaitN(ctx context.Context, n int) error {
	add(c.kind, n)
	return c.Limiter.WaitN(ctx, n)
}

// Wrap returns l with every WaitN counted as traffic of the given kind. Every
// other method (SetLimit, SetBurst, ...) goes to l unchanged, so a limit set
// later, e.g. by a setting-change callback, keeps working.
func Wrap(kind Kind, l stream.Limiter) stream.Limiter {
	return &counting{Limiter: l, kind: kind}
}

// Store is where daily totals live.
type Store interface {
	AddDaily(day string, kind Kind, delta int64) error
	// Sums adds up the days in [from, to]; an empty bound is open.
	Sums(from, to string) (map[string]int64, error)
	// Since lists the per-day counters of the days >= from.
	Since(from string) ([]DayRow, error)
}

// DayRow is one stored (day, kind) counter.
type DayRow struct {
	Day   string
	Kind  string
	Bytes int64
}

var (
	mu    sync.Mutex // guards store, the flush and the reads that add pending to it
	store Store
	now   = time.Now
)

const (
	seriesLen     = 300 // 5 minutes at one point per second
	flushInterval = time.Minute
	dayLayout     = "2006-01-02"
	dailyDays     = 30
)

func today() string { return now().Format(dayLayout) }

// Flush moves the counted-but-unsaved bytes into the store. A failure puts
// them back for the next flush; nothing is dropped.
func Flush() {
	mu.Lock()
	defer mu.Unlock()
	flushLocked()
}

func flushLocked() {
	if store == nil {
		return
	}
	day := today()
	for i, k := range kinds {
		d := global.pending[i].Swap(0)
		if d == 0 {
			continue
		}
		if err := store.AddDaily(day, k, d); err != nil {
			global.pending[i].Add(d)
			log.Errorf("stats: saving %s traffic failed, will retry: %v", k, err)
		}
	}
}

// point is one second of throughput, in bytes per second.
type point struct {
	T int64 `json:"t"`
	Counts
}

var series = struct {
	sync.Mutex
	points [seriesLen]point
	n      int // points written so far; the ring index is n % seriesLen
	last   [len(kinds)]int64
	lastAt time.Time
}{}

// sample records the throughput since the previous call.
func sample(at time.Time) {
	series.Lock()
	defer series.Unlock()
	var cur [len(kinds)]int64
	for i := range kinds {
		cur[i] = global.total[i].Load()
	}
	if series.lastAt.IsZero() {
		series.last, series.lastAt = cur, at
		return
	}
	dt := at.Sub(series.lastAt).Seconds()
	if dt <= 0 {
		return
	}
	p := point{T: at.Unix()}
	for i := range kinds {
		p.set(i, int64(float64(cur[i]-series.last[i])/dt+0.5))
	}
	series.points[series.n%seriesLen] = p
	series.n++
	series.last, series.lastAt = cur, at
}

func seriesSnapshot() []point {
	series.Lock()
	defer series.Unlock()
	n := series.n
	if n > seriesLen {
		n = seriesLen
	}
	out := make([]point, 0, n)
	for i := series.n - n; i < series.n; i++ {
		out = append(out, series.points[i%seriesLen])
	}
	return out
}

var (
	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
)

// Start begins sampling every second and saving every minute into s.
func Start(s Store) {
	runMu.Lock()
	defer runMu.Unlock()
	if cancel != nil {
		return
	}
	mu.Lock()
	store = s
	mu.Unlock()
	ctx, c := context.WithCancel(context.Background())
	cancel, done = c, make(chan struct{})
	go func(done chan struct{}) {
		defer close(done)
		sec := time.NewTicker(time.Second)
		min := time.NewTicker(flushInterval)
		defer sec.Stop()
		defer min.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-sec.C:
				sample(t)
			case <-min.C:
				Flush()
			}
		}
	}(done)
}

// Stop ends the background work and saves what is pending.
func Stop() {
	runMu.Lock()
	c, d := cancel, done
	cancel, done = nil, nil
	runMu.Unlock()
	if c != nil {
		c()
		<-d
	}
	Flush()
}

// Traffic is the traffic part of the overview.
type Traffic struct {
	NowBps Counts   `json:"now_bps"`
	Series []point  `json:"series"`
	Today  Counts   `json:"today"`
	Month  Counts   `json:"month"`
	Total  Counts   `json:"total"`
	Daily  []DayOut `json:"daily"`
}

// DayOut is one day of the last 30, zero-filled.
type DayOut struct {
	Day string `json:"day"`
	Counts
}

// Snapshot is what the dashboard reads: the saved totals plus what has been
// counted since the last flush.
func Snapshot() (Traffic, error) {
	mu.Lock()
	defer mu.Unlock()
	out := Traffic{Series: []point{}, Daily: []DayOut{}}
	if store == nil {
		return out, nil
	}
	t := now()
	day := t.Format(dayLayout)
	var pending Counts
	for i := range kinds {
		pending.set(i, global.pending[i].Load())
	}

	todayRows, err := store.Sums(day, day)
	if err != nil {
		return out, err
	}
	month := t.Format("2006-01")
	monthRows, err := store.Sums(month+"-01", month+"-31")
	if err != nil {
		return out, err
	}
	totalRows, err := store.Sums("", "")
	if err != nil {
		return out, err
	}
	out.Today = byKind(todayRows).plus(pending)
	out.Month = byKind(monthRows).plus(pending)
	out.Total = byKind(totalRows).plus(pending)

	from := t.AddDate(0, 0, -(dailyDays - 1)).Format(dayLayout)
	rows, err := store.Since(from)
	if err != nil {
		return out, err
	}
	perDay := map[string]map[string]int64{}
	for _, r := range rows {
		if perDay[r.Day] == nil {
			perDay[r.Day] = map[string]int64{}
		}
		perDay[r.Day][r.Kind] = r.Bytes
	}
	for i := dailyDays - 1; i >= 0; i-- {
		d := t.AddDate(0, 0, -i).Format(dayLayout)
		c := byKind(perDay[d])
		if d == day {
			c = c.plus(pending)
		}
		out.Daily = append(out.Daily, DayOut{Day: d, Counts: c})
	}

	out.Series = seriesSnapshot()
	if n := len(out.Series); n > 0 {
		out.NowBps = out.Series[n-1].Counts
	}
	return out, nil
}
