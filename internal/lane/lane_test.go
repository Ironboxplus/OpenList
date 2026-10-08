package lane

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestForegroundJumpsASaturatedBackgroundQueue(t *testing.T) {
	l := NewLimiter(100) // a token every 10ms
	bgCtx := WithBackground(context.Background())
	var order atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Wait(bgCtx)
			order.Add(1)
		}()
	}
	time.Sleep(50 * time.Millisecond) // the queue is full and draining
	before := order.Load()
	start := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	waited, served := time.Since(start), order.Load()-before
	if waited > 40*time.Millisecond {
		t.Fatalf("foreground call waited %v behind ~25 queued background calls", waited)
	}
	if served > 2 {
		t.Fatalf("%d background calls were served before the foreground one", served)
	}
	wg.Wait()
}

func TestBackgroundStillGetsOneTokenInFive(t *testing.T) {
	l := NewLimiter(200)
	var fg, bg atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	loop := func(ctx context.Context, n *atomic.Int64) {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if l.Wait(ctx) == nil {
				n.Add(1)
			}
		}
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go loop(context.Background(), &fg)
	}
	wg.Add(1)
	go loop(WithBackground(context.Background()), &bg)
	time.Sleep(1500 * time.Millisecond)
	close(stop)
	wg.Wait()

	total := fg.Load() + bg.Load()
	t.Logf("fg=%d bg=%d", fg.Load(), bg.Load())
	if total < 200 || total > 340 {
		t.Fatalf("%d tokens in 1.5s at 200/s: the total rate is not held", total)
	}
	if share := float64(bg.Load()) / float64(total); share < 0.14 || share > 0.26 {
		t.Fatalf("background share = %.2f, want about 0.20", share)
	}
}

func TestCancelledWaiterLeavesTheQueue(t *testing.T) {
	l := NewLimiter(5) // a token every 200ms
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{context.Background(), WithBackground(context.Background())} {
		ctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- l.Wait(ctx) }()
		time.Sleep(20 * time.Millisecond)
		l.mu.Lock()
		queued := l.fg.Len() + l.bg.Len()
		l.mu.Unlock()
		if queued != 1 {
			t.Fatalf("queued = %d before cancel, want 1", queued)
		}
		cancel()
		select {
		case err := <-done:
			if err != context.Canceled {
				t.Fatalf("Wait = %v, want context.Canceled", err)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("cancelled Wait did not return")
		}
		l.mu.Lock()
		queued = l.fg.Len() + l.bg.Len()
		l.mu.Unlock()
		if queued != 0 {
			t.Fatalf("cancelled waiter still queued (%d)", queued)
		}
	}
	// A cancelled waiter must not cost the next caller a token or the pace.
	start := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Fatalf("next Wait took %v", d)
	}
}

func TestIdleLimiterGrantsImmediatelyThenPaces(t *testing.T) {
	l := NewLimiter(20) // 50ms
	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 120*time.Millisecond || d > 300*time.Millisecond {
		t.Fatalf("4 tokens took %v, want about 150ms (first immediate)", d)
	}
}
