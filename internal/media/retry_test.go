package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "net/http: TLS handshake timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestTransientErrorsAreTheConnectionsNotTheStorages(t *testing.T) {
	wrapped := fmt.Errorf("failed get dir: %w", fmt.Errorf("get_info: %w", timeoutErr{}))
	for _, err := range []error{wrapped, io.ErrUnexpectedEOF, errors.New("read tcp: connection reset by peer"), errors.New("http2: server sent GOAWAY and closed")} {
		if !transient(err) {
			t.Errorf("%v should be retried", err)
		}
	}
	for _, err := range []error{nil, context.Canceled, errors.New("object not found"), errors.New("permission denied")} {
		if transient(err) {
			t.Errorf("%v should not be retried", err)
		}
	}
}

func TestWithRetryTriesAgainAndStops(t *testing.T) {
	old := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { retryDelays = old }()

	n := 0
	v, err := withRetry(context.Background(), func() (int, error) {
		n++
		if n < 3 {
			return 0, timeoutErr{}
		}
		return 7, nil
	})
	if err != nil || v != 7 || n != 3 {
		t.Fatalf("v=%d err=%v after %d calls", v, err, n)
	}
	n = 0
	if _, err := withRetry(context.Background(), func() (int, error) { n++; return 0, errors.New("object not found") }); err == nil || n != 1 {
		t.Fatalf("permanent error: %d calls, %v", n, err)
	}
	n = 0
	if _, err := withRetry(context.Background(), func() (int, error) { n++; return 0, timeoutErr{} }); err == nil || n != 3 {
		t.Fatalf("persistent error: %d calls, %v", n, err)
	}
}
