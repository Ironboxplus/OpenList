package media

import (
	"context"
	"errors"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	log "github.com/sirupsen/logrus"
	"io"
	"strings"
	"time"
)

// A scan reads a lot of directories from storages that sit behind the network
// (115 above all). One TLS handshake timeout on the root listing used to drop
// a whole library, so the reads of a scan are retried when the failure is the
// connection's and not the answer's.
//
// Which failures: rclone's fserrors.ShouldRetry (fs/fserrors/error.go): an error
// in the chain whose Timeout() or Temporary() is true (a TLS handshake timeout
// is one: nothing was sent), io.EOF / io.ErrUnexpectedEOF, and a short list of
// phrases for errors that carry no type (closed connections, GOAWAY, stream
// errors). Added for the same reason: a reset by the peer, a broken pipe, and
// the gateway statuses of a proxy in front of the API. The retries go through
// the driver again, so the driver's own rate gate paces them: one request at a
// time, no change to its limits.

// retryDelays are the waits before the 2nd, 3rd and 4th try (variable for tests).
var retryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

var retriablePhrases = []string{
	"use of closed network connection", "unexpected EOF reading trailer", "transport connection broken",
	"server closed idle connection", "bad record MAC", "stream error:", "http2: server sent GOAWAY",
	"tls: use of closed connection", // rclone's list
	"connection reset by peer", "broken pipe", "TLS handshake timeout", "i/o timeout",
	"bad gateway", "service unavailable", "gateway timeout", // proxies in front of an API
}

// transient reports whether err is the connection failing rather than the
// storage answering no.
func transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if t, ok := e.(interface{ Timeout() bool }); ok && t.Timeout() {
			return true
		}
		if t, ok := e.(interface{ Temporary() bool }); ok && t.Temporary() {
			return true
		}
	}
	msg := err.Error()
	for _, p := range retriablePhrases {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// withRetry runs a read-only call, trying again after a transient failure.
func withRetry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	for i := 0; ; i++ {
		v, err := fn()
		if err == nil || !transient(err) || i >= len(retryDelays) {
			return v, err
		}
		log.Warnf("media scan: retrying after a transient error (%d/%d): %v", i+1, len(retryDelays), err)
		select {
		case <-time.After(retryDelays[i]):
		case <-ctx.Done():
			return v, err
		}
	}
}

// scanList and scanGet are fs.List and fs.Get with the retry of a scan.
func scanList(ctx context.Context, p string, args *fs.ListArgs) ([]model.Obj, error) {
	return withRetry(ctx, func() ([]model.Obj, error) { return fs.List(ctx, p, args) })
}

func scanGet(ctx context.Context, p string, args *fs.GetArgs) (model.Obj, error) {
	return withRetry(ctx, func() (model.Obj, error) { return fs.Get(ctx, p, args) })
}
