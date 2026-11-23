package stream

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/net"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

// The gate also counts the connections themselves. A storage that allows N
// connections per file refuses the N+1th whoever opens it, so every connection
// to the file, a part of a multi-part download or a plain range stream, takes a
// slot before it is opened and gives it back when its body was read to the end
// or closed. With fewer slots free than a download has workers the extra
// workers wait, so the download runs with the slots there are (at least one
// once any is free); nothing fails for it.
//
// A playback's connections (the request's context says so, see WithPlayback)
// do not wait for the others: when none is free they take back the slot of a
// part that can be fetched again (a preempted part reads ErrPreempted and its
// downloader opens it anew from where it stopped, when a slot is free), and
// while one waits no other read takes a freed slot. When what holds the slots
// is the first connections of playbacks alone, a playback's first connection
// goes ahead, as playbacks do (see FileGate). Its other workers (extra) only
// ever take a free slot, so a download runs with fewer workers rather than be
// refused by the storage.

type playbackKey struct{}

// WithPlayback marks the reads made with the returned context as those of a
// playback.
func WithPlayback(ctx context.Context) context.Context {
	return context.WithValue(ctx, playbackKey{}, true)
}

// Conn is one connection's slot of a file.
type Conn struct {
	g         *FileGate
	f         *gateFile
	playback  bool
	resumable bool
	extra     bool
	preempted atomic.Bool
	interrupt func() // under g.mu
	once      sync.Once
}

func (c *Conn) Preempted() bool { return c.preempted.Load() }

func (c *Conn) OnPreempt(interrupt func()) {
	c.g.mu.Lock()
	c.interrupt = interrupt
	already := c.preempted.Load()
	c.g.mu.Unlock()
	if already {
		interrupt()
	}
}

func (c *Conn) Release() {
	c.once.Do(func() {
		c.g.mu.Lock()
		defer c.g.mu.Unlock()
		delete(c.f.conns, c)
		c.f.signal()
	})
}

type noSlot struct{}

func (noSlot) Release()         {}
func (noSlot) Preempted() bool  { return false }
func (noSlot) OnPreempt(func()) {}

type fileConnGate struct {
	g     *FileGate
	key   string
	limit func() int
}

// ConnGate is the gate of the connections to key, on a storage that allows
// limit() connections per file (read at each use; 0 or less: no limit).
func (g *FileGate) ConnGate(key string, limit func() int) model.ConnGate {
	return fileConnGate{g: g, key: key, limit: limit}
}

func (c fileConnGate) AcquireConn(ctx context.Context, resumable, extra bool) (model.ConnSlot, error) {
	n := c.limit()
	if n <= 0 {
		return noSlot{}, nil
	}
	playback, _ := ctx.Value(playbackKey{}).(bool)
	g := c.g
	waiting := false
	for {
		g.mu.Lock()
		f := g.file(c.key)
		var interrupts []func()
		ok := false
		if playback {
			ok = len(f.conns) < n
			if !ok {
				// What stands in the way: background reads, and the extra workers
				// of another playback's download when this is a read's first
				// connection. They are told to give up their slots; the first
				// connection goes beyond the limit only when nothing else is in
				// the way, an extra one never does.
				blocked := false
				for h := range f.conns {
					displaceable := h.resumable && (!h.playback || (h.extra && !extra))
					if !displaceable && h.playback {
						continue
					}
					blocked = true
					if displaceable && h.preempted.CompareAndSwap(false, true) && h.interrupt != nil {
						interrupts = append(interrupts, h.interrupt)
					}
				}
				ok = !blocked && !extra
			}
		} else {
			ok = len(f.conns) < n && f.pbWait == 0
		}
		if ok {
			if waiting {
				f.pbWait--
			}
			h := &Conn{g: g, f: f, playback: playback, resumable: resumable, extra: extra}
			f.conns[h] = struct{}{}
			g.mu.Unlock()
			return h, nil
		}
		if playback && !waiting {
			f.pbWait++
			waiting = true
		}
		changed := f.changed
		g.mu.Unlock()
		for _, interrupt := range interrupts {
			interrupt()
		}

		select {
		case <-changed:
		case <-ctx.Done():
			if waiting {
				g.mu.Lock()
				f.pbWait--
				f.signal()
				g.mu.Unlock()
			}
			return nil, ctx.Err()
		}
	}
}

// gatedBody is a connection's body that gives its slot back at the end.
type gatedBody struct {
	io.ReadCloser
	slot model.ConnSlot
}

func newGatedBody(body io.ReadCloser, slot model.ConnSlot) *gatedBody {
	b := &gatedBody{ReadCloser: body, slot: slot}
	slot.OnPreempt(func() { _ = body.Close() })
	return b
}

func (b *gatedBody) Read(p []byte) (int, error) {
	if b.slot.Preempted() {
		_ = b.ReadCloser.Close()
		b.slot.Release()
		return 0, net.ErrPreempted
	}
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		if err == io.EOF {
			b.slot.Release()
		} else if b.slot.Preempted() {
			_ = b.ReadCloser.Close()
			b.slot.Release()
			err = net.ErrPreempted
		}
	}
	return n, err
}

func (b *gatedBody) Close() error {
	err := b.ReadCloser.Close()
	b.slot.Release()
	return err
}

// gateHttpClient makes every request of client a connection of the gate (nil:
// client as it is).
func gateHttpClient(gate model.ConnGate, client net.HttpRequestFunc) net.HttpRequestFunc {
	if gate == nil {
		return client
	}
	return func(ctx context.Context, params *net.HttpRequestParams) (*http.Response, error) {
		slot, err := gate.AcquireConn(ctx, params.Part, params.Extra)
		if err != nil {
			return nil, err
		}
		resp, err := client(ctx, params)
		if err != nil {
			slot.Release()
			return nil, err
		}
		resp.Body = newGatedBody(resp.Body, slot)
		return resp, nil
	}
}

// gateRangeReader makes every stream of rr a connection of the gate (nil: rr as
// it is). What reads it as a whole stream cannot fetch again, so it is never
// preempted.
func gateRangeReader(gate model.ConnGate, rr model.RangeReaderIF) model.RangeReaderIF {
	if gate == nil {
		return rr
	}
	return RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
		slot, err := gate.AcquireConn(ctx, false, false)
		if err != nil {
			return nil, err
		}
		body, err := rr.RangeRead(ctx, r)
		if err != nil {
			slot.Release()
			return nil, err
		}
		return newGatedBody(body, slot), nil
	})
}
