package syncjob

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPackUploadsBoundedConcurrency(t *testing.T) {
	const limit, total = 3, 12
	u := newPackUploads(context.Background(), limit)
	var inFlight, peak, done atomic.Int32
	for range total {
		if err := u.start(context.Background(), func(context.Context) error {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			inFlight.Add(-1)
			done.Add(1)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := u.wait(); err != nil {
		t.Fatal(err)
	}
	if done.Load() != total {
		t.Fatalf("%d of %d uploads ran", done.Load(), total)
	}
	if peak.Load() != limit {
		t.Fatalf("peak in flight %d, want %d", peak.Load(), limit)
	}
}

func TestPackUploadsReusableAfterWait(t *testing.T) {
	u := newPackUploads(context.Background(), 2)
	for range 2 {
		var n atomic.Int32
		for range 4 {
			if err := u.start(context.Background(), func(context.Context) error { n.Add(1); return nil }); err != nil {
				t.Fatal(err)
			}
		}
		if err := u.wait(); err != nil || n.Load() != 4 {
			t.Fatalf("round: err=%v ran=%d", err, n.Load())
		}
	}
}

func TestPackUploadsFailureStopsTheRest(t *testing.T) {
	boom := errors.New("boom")
	u := newPackUploads(context.Background(), 2)

	var wg sync.WaitGroup
	wg.Add(1)
	sibling := make(chan error, 1)
	if err := u.start(context.Background(), func(ctx context.Context) error {
		wg.Done()
		<-ctx.Done() // runs until the other upload fails
		sibling <- context.Cause(ctx)
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	_ = u.start(context.Background(), func(context.Context) error { return boom })

	if err := <-sibling; !errors.Is(err, boom) {
		t.Fatalf("sibling stopped by %v, want boom", err)
	}
	// The builder is told as soon as it asks for the next slot.
	if err := u.start(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, boom) {
		t.Fatalf("start after failure = %v, want boom", err)
	}
	if err := u.wait(); !errors.Is(err, boom) {
		t.Fatalf("wait = %v, want boom", err)
	}
	// wait ended the batch: the next one starts clean.
	if err := u.start(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("start in the next batch = %v", err)
	}
	if err := u.wait(); err != nil {
		t.Fatalf("next batch: %v", err)
	}
}

func TestPackUploadsFollowTheBuildersContext(t *testing.T) {
	u := newPackUploads(context.Background(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	if err := u.start(ctx, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	if err := u.wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait = %v, want canceled", err)
	}
}
