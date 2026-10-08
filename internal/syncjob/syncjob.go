// Package syncjob runs rclone copy / sync / bisync / check jobs between a
// local directory and OpenList mount paths, or between two mount paths.
//
// Jobs run inside the server process on purpose. A separate CLI process would
// open its own 115 client: refreshing a token there rotates the refresh token
// out from under the server's copy, and each process would pace API calls with
// its own rate limiter, so together they exceed the limit. The CLI only asks
// the running server to start a job, follows it and controls it.
package syncjob

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/rclonefs"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils/random"
	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/cmd/bisync"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/rc"
	rsync "github.com/rclone/rclone/fs/sync"
	log "github.com/sirupsen/logrus"
)

type Mode string

const (
	// ModeCopy adds and updates files at dst, never deletes.
	ModeCopy Mode = "copy"
	// ModeSync makes dst identical to src, deleting extras at dst.
	ModeSync Mode = "sync"
	// ModeBisync propagates changes both ways; the first run needs Resync.
	ModeBisync Mode = "bisync"
	// ModeCheck only compares, reporting differences as errors.
	ModeCheck Mode = "check"
)

// LocalPrefix marks an endpoint as a directory on the server's own disk;
// anything starting with "/" is an OpenList mount path.
const LocalPrefix = "local:"

type Spec struct {
	Mode Mode   `json:"mode"`
	Src  string `json:"src"`
	Dst  string `json:"dst"`
	// DryRun reports what would change without changing anything.
	DryRun bool `json:"dry_run"`
	// SizeOnly compares by size alone. The default compares by hash, because
	// cloud drives can't keep modification times; size-only skips hashing
	// local files at the cost of missing same-size edits.
	SizeOnly bool `json:"size_only"`
	// TrackRenames turns a rename at src into a server-side move at dst.
	TrackRenames bool `json:"track_renames"`
	// Resync (bisync) rebuilds the baseline. Required on the first run.
	Resync bool `json:"resync"`
	// Force (bisync) bypasses the safety check that aborts a run deleting
	// more than half the files.
	Force bool `json:"force"`
	// Filters are rclone filter rules, e.g. "- *.tmp", "+ /movies/**".
	Filters []string `json:"filters"`
	// Transfers and Checkers bound how many files are uploaded and compared
	// at once; 0 keeps rclone's defaults (4 and 8). Every storage still paces
	// its API calls with its own rate limiter whatever these are.
	Transfers int `json:"transfers"`
	Checkers  int `json:"checkers"`
	// MaxDelete aborts a sync that would delete more than this many files at
	// dst. Unset means no limit.
	MaxDelete *int64 `json:"max_delete,omitempty"`
}

type State string

const (
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCanceled  State = "canceled"
)

// Snapshot is a job as reported over the API.
type Snapshot struct {
	ID        string    `json:"id"`
	Spec      Spec      `json:"spec"`
	State     State     `json:"state"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	// Counters classify every file the job looked at; see Counters.
	Counters Counters `json:"counters"`
	// Stats is rclone's own accounting for the job: bytes, speed, eta,
	// transfers, checks, deletes, renames, errors, and the files in flight
	// with their individual progress.
	Stats map[string]any `json:"stats,omitempty"`
	// Events is the per-file log, newest last; only filled by Get.
	Events []Event `json:"events,omitempty"`
}

type job struct {
	mu     sync.Mutex
	snap   Snapshot
	log    eventLog
	cancel context.CancelFunc
	group  string
}

func (j *job) snapshot(ctx context.Context, withEvents bool) Snapshot {
	j.mu.Lock()
	s := j.snap
	s.Counters = j.log.counters
	if withEvents {
		s.Events = j.log.events()
	}
	j.mu.Unlock()
	if stats, err := accounting.StatsGroup(ctx, j.group).RemoteStats(false); err == nil {
		s.Stats = stats
	}
	return s
}

// Manager keeps running jobs and the most recent finished ones.
type Manager struct {
	mu   sync.Mutex
	jobs map[string]*job
	// history is where finished jobs are appended; "" disables it.
	history string
	// run is swappable so tests can drive the job lifecycle without rclone.
	run func(ctx context.Context, spec Spec, src, dst fs.Fs) error
}

// keepFinished bounds how many finished jobs stay queryable in memory; the
// history file keeps all of them.
const keepFinished = 50

var Default = NewManager()

func NewManager() *Manager {
	return &Manager{jobs: map[string]*job{}, run: run}
}

func (m *Manager) historyPath() string {
	if m.history != "" {
		return m.history
	}
	return filepath.Join(flags.DataDir, "log", "sync-jobs.jsonl")
}

// SetHistoryPath points the finished-job history at path.
func (m *Manager) SetHistoryPath(path string) { m.history = path }

// Start validates spec, resolves both endpoints and starts the job. Problems
// with the request itself are returned here rather than as a failed job.
func (m *Manager) Start(spec Spec) (Snapshot, error) {
	switch spec.Mode {
	case ModeCopy, ModeSync, ModeBisync, ModeCheck:
	default:
		return Snapshot{}, fmt.Errorf("unknown mode %q (want copy, sync, bisync or check)", spec.Mode)
	}
	if spec.Transfers < 0 || spec.Checkers < 0 {
		return Snapshot{}, fmt.Errorf("transfers and checkers can't be negative")
	}
	fo := newFilterOpt(spec.Filters)
	if _, err := filter.NewFilter(&fo); err != nil {
		return Snapshot{}, fmt.Errorf("filters: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	src, err := resolve(ctx, spec.Src)
	if err != nil {
		cancel()
		return Snapshot{}, fmt.Errorf("src: %w", err)
	}
	dst, err := resolve(ctx, spec.Dst)
	if err != nil {
		cancel()
		return Snapshot{}, fmt.Errorf("dst: %w", err)
	}
	id := random.String(12)
	j := &job{
		snap:   Snapshot{ID: id, Spec: spec, State: StateRunning, StartedAt: time.Now()},
		cancel: cancel,
		group:  "openlist-sync-" + id,
	}
	ctx = accounting.WithStatsGroup(ctx, j.group)
	ctx = operations.WithLogger(ctx, func(_ context.Context, sigil operations.Sigil, src, dst fs.DirEntry, err error) {
		j.mu.Lock()
		j.log.compared(sigil, src, dst, err)
		j.mu.Unlock()
	})
	ctx = rclonefs.WithUploadObserver(ctx, func(u rclonefs.Upload) {
		j.mu.Lock()
		j.log.uploaded(u)
		j.mu.Unlock()
	})
	m.mu.Lock()
	m.jobs[id] = j
	m.pruneLocked()
	m.mu.Unlock()
	log.Infof("[sync] job %s started: %s %s -> %s%s", id, spec.Mode, spec.Src, spec.Dst, dryRunNote(spec))

	go func() {
		err := m.run(ctx, spec, src, dst)
		j.mu.Lock()
		j.snap.EndedAt = time.Now()
		switch {
		case err == nil:
			j.snap.State = StateSucceeded
		case errors.Is(ctx.Err(), context.Canceled):
			j.snap.State = StateCanceled
			j.snap.Error = err.Error()
		default:
			j.snap.State = StateFailed
			j.snap.Error = err.Error()
		}
		j.mu.Unlock()
		final := j.snapshot(ctx, false)
		cancel()
		m.finished(final)
	}()
	return j.snapshot(ctx, false), nil
}

func dryRunNote(spec Spec) string {
	if spec.DryRun {
		return " (dry run)"
	}
	return ""
}

func (m *Manager) finished(s Snapshot) {
	c := s.Counters
	line := fmt.Sprintf("[sync] job %s %s after %s: %d uploaded (%d rapid), %d differ, %d only at src, %d only at dst, %d errors",
		s.ID, s.State, s.EndedAt.Sub(s.StartedAt).Round(time.Second),
		c.Uploaded, c.RapidUploads, c.Differ, c.OnlyInSrc, c.OnlyInDst, c.Errors)
	if s.State == StateSucceeded {
		log.Info(line)
	} else {
		log.Warnf("%s: %s", line, s.Error)
	}
	if err := appendHistory(m.historyPath(), s); err != nil {
		log.Warnf("[sync] failed to record job %s in history: %v", s.ID, err)
	}
}

// Get returns a job; withEvents adds its per-file log.
func (m *Manager) Get(id string, withEvents bool) (Snapshot, bool) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return Snapshot{}, false
	}
	return j.snapshot(context.Background(), withEvents), true
}

// List returns the jobs still in memory, newest first.
func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	jobs := make([]*job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	out := make([]Snapshot, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.snapshot(context.Background(), false))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

// History returns up to limit finished jobs from the history file, newest
// first, including ones from before the last restart.
func (m *Manager) History(limit int) ([]Snapshot, error) {
	return readHistory(m.historyPath(), limit)
}

func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if ok {
		j.mu.Lock()
		running := j.snap.State == StateRunning
		j.mu.Unlock()
		if running {
			log.Infof("[sync] job %s cancel requested", id)
		}
		j.cancel()
	}
	return ok
}

// SetBwLimit caps the bandwidth of all sync jobs together, e.g. "10M" for
// 10 MiB/s, "10M:2M" for separate upload:download caps, or "off". It takes
// effect immediately, including on jobs already running.
func SetBwLimit(rate string) error {
	var tt fs.BwTimetable
	if err := tt.Set(rate); err != nil {
		return fmt.Errorf("bad bandwidth limit %q: %w", rate, err)
	}
	if len(tt) != 1 {
		return fmt.Errorf("bad bandwidth limit %q: want a single rate, not a timetable", rate)
	}
	accounting.TokenBucket.SetBwLimit(tt[0].Bandwidth)
	return nil
}

// BwLimit reports the current limit in bytes per second, -1 when unlimited.
func BwLimit() (int64, error) {
	call := rc.Calls.Get("core/bwlimit")
	if call == nil {
		return -1, errors.New("bandwidth limiter unavailable")
	}
	out, err := call.Fn(context.Background(), rc.Params{})
	if err != nil {
		return -1, err
	}
	return out.GetInt64("bytesPerSecond")
}

// pruneLocked drops the oldest finished jobs, and their rclone stats groups,
// beyond keepFinished.
func (m *Manager) pruneLocked() {
	var finished []*job
	for _, j := range m.jobs {
		j.mu.Lock()
		if j.snap.State != StateRunning {
			finished = append(finished, j)
		}
		j.mu.Unlock()
	}
	if len(finished) <= keepFinished {
		return
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].snap.EndedAt.Before(finished[b].snap.EndedAt) })
	for _, j := range finished[:len(finished)-keepFinished] {
		delete(m.jobs, j.snap.ID)
		if call := rc.Calls.Get("core/stats-delete"); call != nil {
			_, _ = call.Fn(context.Background(), rc.Params{"group": j.group})
		}
	}
}

// resolve turns an endpoint into an rclone Fs: "local:<absolute dir>" is a
// directory on this machine, "/<mount path>" is an OpenList storage.
func resolve(ctx context.Context, endpoint string) (fs.Fs, error) {
	if dir, ok := strings.CutPrefix(endpoint, LocalPrefix); ok {
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("local path %q must be absolute", dir)
		}
		return local.NewFs(ctx, "local", filepath.Clean(dir), configmap.Simple{})
	}
	if strings.HasPrefix(endpoint, "/") {
		if _, _, err := op.GetStorageAndActualPath(endpoint); err != nil {
			return nil, fmt.Errorf("no storage mounted at %q: %w", endpoint, err)
		}
		return rclonefs.NewFs(ctx, endpoint)
	}
	return nil, fmt.Errorf("endpoint %q must be an OpenList path starting with / or %s<absolute dir>", endpoint, LocalPrefix)
}

func run(ctx context.Context, spec Spec, src, dst fs.Fs) error {
	ctx, ci := fs.AddConfig(ctx)
	// rclone reads ahead of every transfer's source stream into a buffer.
	// rclonefs.Put range-reads the source itself and never consumes that
	// stream, so read-ahead would only fetch bytes nobody uses.
	ci.BufferSize = 0
	ci.DryRun = spec.DryRun
	ci.SizeOnly = spec.SizeOnly
	ci.CheckSum = !spec.SizeOnly
	ci.TrackRenames = spec.TrackRenames
	if spec.Transfers > 0 {
		ci.Transfers = spec.Transfers
	}
	if spec.Checkers > 0 {
		ci.Checkers = spec.Checkers
	}
	if spec.MaxDelete != nil {
		ci.MaxDelete = *spec.MaxDelete
	}

	fopt := newFilterOpt(spec.Filters)
	fi, err := filter.NewFilter(&fopt)
	if err != nil {
		return fmt.Errorf("filters: %w", err)
	}
	ctx = filter.ReplaceConfig(ctx, fi)

	switch spec.Mode {
	case ModeCopy:
		return rsync.CopyDir(ctx, dst, src, true)
	case ModeSync:
		return rsync.Sync(ctx, dst, src, true)
	case ModeCheck:
		return operations.Check(ctx, &operations.CheckOpt{Fdst: dst, Fsrc: src})
	case ModeBisync:
		compare := "size,checksum"
		if spec.SizeOnly {
			compare = "size"
		}
		return bisync.Bisync(ctx, src, dst, &bisync.Options{
			Resync:             spec.Resync,
			DryRun:             spec.DryRun,
			Force:              spec.Force,
			CreateEmptySrcDirs: true,
			MaxDelete:          bisync.DefaultMaxDelete,
			CompareFlag:        compare,
			Workdir:            filepath.Join(flags.DataDir, "bisync"),
			// A run interrupted by a restart should pick up where it was
			// instead of demanding a manual --resync.
			Recover: true,
		})
	}
	return fmt.Errorf("unknown mode %q", spec.Mode)
}

// NewFilter parses rclone filter rules ("- SPs/**", "+ *.mkv") into a filter,
// for callers outside a sync that want the same syntax.
func NewFilter(rules []string) (*filter.Filter, error) {
	fopt := newFilterOpt(rules)
	return filter.NewFilter(&fopt)
}

// newFilterOpt copies rclone's default filter options (MinAge/MaxAge are
// DurationOff there) instead of using the zero value, whose 0s/0s ages make
// NewFilter fail whenever the two clock reads differ (e.g. on Linux).
func newFilterOpt(rules []string) filter.Options {
	fopt := filter.Opt
	fopt.FilterRule = rules
	return fopt
}
