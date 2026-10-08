// Package lane is a rate limiter with two queues. Bulk work (sync jobs)
// marks its context with WithBackground; everything else is foreground. When a
// token frees up a waiting foreground call goes first, so a person browsing the
// UI isn't stuck behind thousands of queued sync requests, while the total rate
// stays what the limiter was built with.
package lane

import (
	"container/list"
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type backgroundKey struct{}

// WithBackground marks ctx as bulk work: its rate-limited calls queue behind
// foreground ones.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackground reports whether ctx was marked with WithBackground.
func IsBackground(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundKey{}).(bool)
	return v
}

// BackgroundShare: while both lanes wait, background gets one token in this
// many, so a busy foreground can't stall bulk work forever.
const BackgroundShare = 5

type waiter struct {
	ch      chan struct{}
	granted bool
	elem    *list.Element
	bg      bool
}

// Limiter hands out one token every 1/rate seconds (burst 1), foreground
// waiters first.
type Limiter struct {
	interval time.Duration

	mu       sync.Mutex
	next     time.Time // earliest time of the next token
	fg, bg   list.List
	fgStreak int  // foreground grants since the last background one
	running  bool // a dispatcher goroutine is serving the queues
}

// NewLimiter returns a limiter for r tokens per second.
func NewLimiter(r rate.Limit) *Limiter {
	return &Limiter{interval: time.Duration(float64(time.Second) / float64(r))}
}

// Wait blocks until a token is granted or ctx ends. The lane comes from ctx.
func (l *Limiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	now := time.Now()
	if l.fg.Len() == 0 && l.bg.Len() == 0 && !now.Before(l.next) {
		l.next = now.Add(l.interval)
		l.mu.Unlock()
		return nil
	}
	w := &waiter{ch: make(chan struct{}), bg: IsBackground(ctx)}
	if w.bg {
		w.elem = l.bg.PushBack(w)
	} else {
		w.elem = l.fg.PushBack(w)
	}
	if !l.running {
		l.running = true
		go l.dispatch()
	}
	l.mu.Unlock()

	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		defer l.mu.Unlock()
		if w.granted {
			// The token was handed over as the context ended; it is spent
			// either way, and the caller was released.
			return nil
		}
		if w.bg {
			l.bg.Remove(w.elem)
		} else {
			l.fg.Remove(w.elem)
		}
		return ctx.Err()
	}
}

// dispatch grants tokens to waiters at the limiter's rate until none are left.
func (l *Limiter) dispatch() {
	for {
		l.mu.Lock()
		if l.fg.Len() == 0 && l.bg.Len() == 0 {
			l.running = false
			l.mu.Unlock()
			return
		}
		wait := time.Until(l.next)
		l.mu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
		}

		l.mu.Lock()
		w := l.pick()
		if w == nil {
			l.mu.Unlock()
			continue // everyone left while we slept
		}
		w.granted = true
		l.next = time.Now().Add(l.interval)
		close(w.ch)
		l.mu.Unlock()
	}
}

// pick removes and returns the next waiter to serve: foreground first, except
// that after BackgroundShare-1 foreground grants in a row a waiting background
// call gets its turn. The caller holds l.mu.
func (l *Limiter) pick() *waiter {
	if l.bg.Len() == 0 {
		l.fgStreak = 0
	}
	useBG := l.bg.Len() > 0 && (l.fg.Len() == 0 || l.fgStreak >= BackgroundShare-1)
	var e *list.Element
	if useBG {
		e = l.bg.Front()
		l.bg.Remove(e)
		l.fgStreak = 0
	} else if l.fg.Len() > 0 {
		e = l.fg.Front()
		l.fg.Remove(e)
		l.fgStreak++
	} else {
		return nil
	}
	return e.Value.(*waiter)
}
