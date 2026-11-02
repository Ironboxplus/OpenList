package frp

import (
	"strings"
	"sync"
	"time"
)

const ringLines = 300

// Ring is a bounded in-memory log: the last ringLines lines, with the secrets
// of the current configuration replaced by "***". It is where frp's own logger
// writes, so nothing is left on disk and nothing grows.
type Ring struct {
	mu      sync.Mutex
	lines   []string
	secrets []string
	partial string
}

// SetSecrets sets the strings that must never be stored or returned.
func (r *Ring) SetSecrets(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = r.secrets[:0]
	for _, s := range secrets {
		if s != "" {
			r.secrets = append(r.secrets, s)
		}
	}
}

// Redact replaces the secrets in s.
func (r *Ring) Redact(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.redactLocked(s)
}

func (r *Ring) redactLocked(s string) string {
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, "***")
	}
	return s
}

// Write implements io.Writer; it never fails.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	text := r.partial + string(p)
	r.partial = ""
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			break
		}
		r.addLocked(text[:i])
		text = text[i+1:]
	}
	if len(text) > 4096 { // a line without end: do not hold it forever
		r.addLocked(text)
		text = ""
	}
	r.partial = text
	return len(p), nil
}

// Addf adds a line of our own (not frp's).
func (r *Ring) Addf(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addLocked(time.Now().Format("2006-01-02 15:04:05") + " [openlist] " + line)
}

func (r *Ring) addLocked(line string) {
	line = strings.TrimRight(r.redactLocked(line), "\r")
	if line == "" {
		return
	}
	r.lines = append(r.lines, line)
	if len(r.lines) > ringLines {
		r.lines = append(r.lines[:0], r.lines[len(r.lines)-ringLines:]...)
	}
}

// Tail returns the last limit lines (all if limit <= 0 or too large).
func (r *Ring) Tail(limit int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > len(r.lines) {
		limit = len(r.lines)
	}
	return append([]string(nil), r.lines[len(r.lines)-limit:]...)
}
