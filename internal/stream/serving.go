package stream

import (
	"sync"
	"time"
)

// What OpenList itself is serving, by VFS path: a download or proxy request, or a
// redirect to the storage's own link, each of which starts a viewer reading the
// file. Background work that reads the same file over the storage's one
// connection per file (the subtitle prefetch) uses it to stay out of the way. It
// knows only what passed through here: a viewer on a link that was handed out
// earlier is seen as long as the grace lasts, no longer.
var serving struct {
	sync.Mutex
	at map[string]time.Time
}

// MarkServing records that path is being read by a viewer now.
func MarkServing(path string) {
	now := time.Now()
	serving.Lock()
	defer serving.Unlock()
	if serving.at == nil {
		serving.at = map[string]time.Time{}
	}
	serving.at[path] = now
	if len(serving.at) > 4096 {
		for p, t := range serving.at {
			if now.Sub(t) > time.Hour {
				delete(serving.at, p)
			}
		}
	}
}

// ServedWithin is true when path was marked within the last d.
func ServedWithin(path string, d time.Duration) bool {
	serving.Lock()
	defer serving.Unlock()
	t, ok := serving.at[path]
	return ok && time.Since(t) < d
}
