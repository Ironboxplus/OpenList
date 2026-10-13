package stats

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// rateLimiter adapts *rate.Limiter to stream.Limiter (it already has every method).
func newRate(l rate.Limit, burst int) *rate.Limiter { return rate.NewLimiter(l, burst) }

type memStore struct {
	mu   sync.Mutex
	rows map[string]int64 // day|kind
	fail bool
}

func newMem() *memStore { return &memStore{rows: map[string]int64{}} }

func (m *memStore) AddDaily(day string, kind Kind, delta int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("db down")
	}
	m.rows[day+"|"+string(kind)] += delta
	return nil
}

func (m *memStore) Sums(from, to string) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int64{}
	for k, v := range m.rows {
		day, kind := k[:10], k[11:]
		if (from == "" || day >= from) && (to == "" || day <= to) {
			out[kind] += v
		}
	}
	return out, nil
}

func (m *memStore) Since(from string) ([]DayRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DayRow
	for k, v := range m.rows {
		if day := k[:10]; day >= from {
			out = append(out, DayRow{Day: day, Kind: k[11:], Bytes: v})
		}
	}
	return out, nil
}

// reset puts the package back to a clean state for a test.
func reset(t *testing.T, s Store, at time.Time) {
	t.Helper()
	Stop()
	for i := range kinds {
		global.total[i].Store(0)
		global.pending[i].Store(0)
	}
	series.Lock()
	series.n, series.last, series.lastAt = 0, [len(kinds)]int64{}, time.Time{}
	series.Unlock()
	mu.Lock()
	store = s
	now = func() time.Time { return at }
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		store, now = nil, time.Now
		mu.Unlock()
	})
}

var noon = time.Date(2026, 10, 13, 12, 0, 0, 0, time.Local)

func TestWrapCountsUnderInfiniteLimit(t *testing.T) {
	reset(t, newMem(), noon)
	l := Wrap(ServerDownload, newRate(rate.Inf, 0))
	for i := 0; i < 3; i++ {
		if err := l.WaitN(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
	}
	if got := global.pending[0].Load(); got != 3000 {
		t.Fatalf("server_download = %d, want 3000", got)
	}
	if got := global.pending[1].Load(); got != 0 {
		t.Fatalf("server_upload = %d, want 0", got)
	}
}

func TestWrapForwardsSetLimitSoLimitingStillWorks(t *testing.T) {
	reset(t, newMem(), noon)
	inner := newRate(rate.Inf, 0)
	l := Wrap(ClientUpload, inner)
	l.SetLimit(100) // bytes per second
	l.SetBurst(100)
	if inner.Limit() != 100 || l.Limit() != 100 || l.Burst() != 100 {
		t.Fatalf("limit not forwarded: inner %v wrapper %v burst %d", inner.Limit(), l.Limit(), l.Burst())
	}
	// Draining the burst then asking for more must block until the context ends.
	if err := l.WaitN(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.WaitN(ctx, 100); err == nil {
		t.Fatal("WaitN should have been limited (it would need ~1s)")
	}
	// Both attempts moved bytes the caller already had; both are counted.
	if got := global.pending[3].Load(); got != 200 {
		t.Fatalf("client_upload = %d, want 200", got)
	}
}

func TestWrapConcurrentSumIsExact(t *testing.T) {
	reset(t, newMem(), noon)
	l := Wrap(ServerUpload, newRate(rate.Inf, 0))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				_ = l.WaitN(context.Background(), 7)
			}
		}()
	}
	wg.Wait()
	if got := global.pending[1].Load(); got != 8*1000*7 {
		t.Fatalf("sum = %d, want %d", got, 8*1000*7)
	}
}

func TestFlushAddsToStoreAndZeroesPending(t *testing.T) {
	m := newMem()
	reset(t, m, noon)
	add(ServerDownload, 500)
	add(ClientDownload, 200)
	Flush()
	if m.rows["2026-10-13|server_download"] != 500 || m.rows["2026-10-13|client_download"] != 200 {
		t.Fatalf("rows = %v", m.rows)
	}
	for i := range kinds {
		if p := global.pending[i].Load(); p != 0 {
			t.Fatalf("pending[%d] = %d after flush", i, p)
		}
	}
	// A second flush adds on top (upsert, not replace).
	add(ServerDownload, 100)
	Flush()
	if m.rows["2026-10-13|server_download"] != 600 {
		t.Fatalf("rows = %v", m.rows)
	}
}

func TestFlushFailureKeepsTheDelta(t *testing.T) {
	m := newMem()
	reset(t, m, noon)
	m.fail = true
	add(ServerUpload, 900)
	Flush()
	if got := global.pending[1].Load(); got != 900 {
		t.Fatalf("pending after failed flush = %d, want 900", got)
	}
	m.fail = false
	add(ServerUpload, 100)
	Flush()
	if m.rows["2026-10-13|server_upload"] != 1000 || global.pending[1].Load() != 0 {
		t.Fatalf("rows = %v pending = %d", m.rows, global.pending[1].Load())
	}
}

func TestFlushBooksOnTheDayOfTheFlush(t *testing.T) {
	m := newMem()
	reset(t, m, time.Date(2026, 10, 13, 23, 59, 30, 0, time.Local))
	add(ServerDownload, 10)
	// midnight passes before the flush: the whole delta goes to the new day
	mu.Lock()
	now = func() time.Time { return time.Date(2026, 10, 14, 0, 0, 20, 0, time.Local) }
	mu.Unlock()
	Flush()
	if m.rows["2026-10-14|server_download"] != 10 || m.rows["2026-10-13|server_download"] != 0 {
		t.Fatalf("rows = %v", m.rows)
	}
}

func TestSnapshotAddsPendingToSavedTotals(t *testing.T) {
	m := newMem()
	reset(t, m, noon)
	m.rows["2026-10-13|server_download"] = 1000 // today, saved
	m.rows["2026-10-02|server_download"] = 4000 // this month
	m.rows["2026-09-30|server_download"] = 8000 // last month, in the last 30 days
	m.rows["2026-01-01|server_download"] = 16000
	add(ServerDownload, 5)
	snap, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Today.ServerDownload != 1005 {
		t.Fatalf("today = %d, want 1005", snap.Today.ServerDownload)
	}
	if snap.Month.ServerDownload != 5005 {
		t.Fatalf("month = %d, want 5005", snap.Month.ServerDownload)
	}
	if snap.Total.ServerDownload != 29005 {
		t.Fatalf("total = %d, want 29005", snap.Total.ServerDownload)
	}
	if len(snap.Daily) != 30 || snap.Daily[29].Day != "2026-10-13" || snap.Daily[0].Day != "2026-09-14" {
		t.Fatalf("daily = %d rows %s..%s", len(snap.Daily), snap.Daily[0].Day, snap.Daily[len(snap.Daily)-1].Day)
	}
	if snap.Daily[29].ServerDownload != 1005 {
		t.Fatalf("today in daily = %d", snap.Daily[29].ServerDownload)
	}
	var sep30 int64
	for _, d := range snap.Daily {
		if d.Day == "2026-09-30" {
			sep30 = d.ServerDownload
		}
	}
	if sep30 != 8000 {
		t.Fatalf("2026-09-30 = %d, want 8000 (a gap day is zero-filled, a stored day is kept)", sep30)
	}
	if snap.Series == nil || snap.Daily == nil {
		t.Fatal("empty collections must serialise as [], not null")
	}
}

func TestSampleKeepsFiveMinutesOfSpeed(t *testing.T) {
	reset(t, newMem(), noon)
	base := time.Unix(1_760_000_000, 0)
	sample(base) // primes
	for i := 1; i <= 310; i++ {
		add(ServerDownload, 1000*i%7+1) // some bytes every second
		sample(base.Add(time.Duration(i) * time.Second))
	}
	pts := seriesSnapshot()
	if len(pts) != 300 {
		t.Fatalf("points = %d, want 300", len(pts))
	}
	if pts[0].T >= pts[299].T {
		t.Fatal("points are not in time order")
	}
	if pts[299].T != base.Add(310*time.Second).Unix() {
		t.Fatalf("newest point at %d", pts[299].T)
	}
}

func TestSampleComputesBytesPerSecond(t *testing.T) {
	reset(t, newMem(), noon)
	base := time.Unix(1_760_000_000, 0)
	sample(base)
	add(ServerDownload, 3000)
	add(ClientUpload, 50)
	sample(base.Add(2 * time.Second)) // 2s elapsed
	snap, _ := Snapshot()
	if snap.NowBps.ServerDownload != 1500 || snap.NowBps.ClientUpload != 25 {
		t.Fatalf("now_bps = %+v", snap.NowBps)
	}
}
