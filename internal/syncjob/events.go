package syncjob

import (
	"log/slog"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/rclonefs"
	"github.com/rclone/rclone/fs"
	rlog "github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/fs/operations"
	log "github.com/sirupsen/logrus"
)

// Counters classify the files a job looked at. The comparison counters use
// rclone's own verdicts, so their meaning follows the mode: in copy/sync a
// file only in src is about to be uploaded and (sync) a file only in dst is
// about to be deleted; in check they are simply differences.
type Counters struct {
	Match     int64 `json:"match"`
	Differ    int64 `json:"differ"`
	OnlyInSrc int64 `json:"only_in_src"`
	OnlyInDst int64 `json:"only_in_dst"`
	Errors    int64 `json:"errors"`
	// Uploaded counts files written into OpenList storages; RapidUploads of
	// them the drive accepted by hash without the content being transferred,
	// and RapidBytes is the size of those — bandwidth not spent.
	Uploaded      int64 `json:"uploaded"`
	UploadedBytes int64 `json:"uploaded_bytes"`
	RapidUploads  int64 `json:"rapid_uploads"`
	RapidBytes    int64 `json:"rapid_bytes"`
}

type Event struct {
	Time time.Time `json:"time"`
	// Kind is one of: only_in_src, only_in_dst, differ, error, uploaded, rapid.
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Size  int64  `json:"size,omitempty"`
	Error string `json:"error,omitempty"`
}

// maxEvents bounds the per-job log; counters keep exact totals regardless.
const maxEvents = 1000

// eventLog is a job's per-file record. Callers hold the job's lock.
type eventLog struct {
	counters Counters
	ring     []Event
	next     int
}

func (l *eventLog) add(e Event) {
	e.Time = time.Now()
	if len(l.ring) < maxEvents {
		l.ring = append(l.ring, e)
		return
	}
	l.ring[l.next] = e
	l.next = (l.next + 1) % maxEvents
}

func (l *eventLog) events() []Event {
	out := make([]Event, 0, len(l.ring))
	out = append(out, l.ring[l.next:]...)
	return append(out, l.ring[:l.next]...)
}

// compared records one of rclone's per-file verdicts. Matches are only
// counted: on an incremental run nearly every file matches, and logging each
// would push the interesting events out of the ring.
func (l *eventLog) compared(sigil operations.Sigil, src, dst fs.DirEntry, err error) {
	entry := dst
	if entry == nil {
		entry = src
	}
	e := Event{}
	if entry != nil {
		e.Path, e.Size = entry.Remote(), entry.Size()
	}
	switch sigil {
	case operations.Match:
		l.counters.Match++
		return
	case operations.Differ:
		l.counters.Differ++
		e.Kind = "differ"
	case operations.MissingOnDst:
		l.counters.OnlyInSrc++
		e.Kind = "only_in_src"
	case operations.MissingOnSrc:
		l.counters.OnlyInDst++
		e.Kind = "only_in_dst"
	case operations.TransferError:
		l.counters.Errors++
		e.Kind = "error"
	default:
		return
	}
	if err != nil {
		e.Error = err.Error()
	}
	l.add(e)
}

func (l *eventLog) uploaded(u rclonefs.Upload) {
	e := Event{Path: u.Remote, Size: u.Size, Kind: "uploaded"}
	switch {
	case u.Err != nil:
		// rclone retries failed transfers and reports the final outcome
		// through compared(); this records each attempt's reason.
		e.Kind, e.Error = "error", u.Err.Error()
	case u.Rapid():
		l.counters.Uploaded++
		l.counters.RapidUploads++
		l.counters.RapidBytes += u.Size
		e.Kind = "rapid"
	default:
		l.counters.Uploaded++
		l.counters.UploadedBytes += u.Size
	}
	l.add(e)
}

// rclone logs through its own slog handler straight to stderr. Route it into
// OpenList's log instead, so a sync job's warnings and errors land next to
// everything else the server logs.
func init() {
	rlog.Handler.SetOutput(func(level slog.Level, text string) {
		text = "[sync] " + strings.TrimRight(text, "\n")
		switch {
		case level >= slog.LevelError:
			log.Error(text)
		case level >= slog.LevelWarn:
			log.Warn(text)
		case level >= slog.LevelInfo:
			log.Info(text)
		default:
			log.Debug(text)
		}
	})
}
