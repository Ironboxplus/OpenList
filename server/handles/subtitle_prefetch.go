package handles

import (
	"context"
	stdpath "path"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/media"
	"github.com/OpenListTeam/OpenList/v4/internal/media/subextract"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// The library prefetch (setting subtitle_prefetch_library, off by default) reads
// the embedded subtitle tracks no provider file stands in for out of every video
// of the media library, one file at a time and behind any extraction a viewer
// asks for, so that the first play of a film finds its tracks on disk.

const (
	prefetchPoll     = 5 * time.Second
	prefetchRepeat   = time.Hour
	prefetchBusyWait = 2 * time.Second
	// prefetchStreamGrace is how long after OpenList last handed a file to a
	// viewer the prefetch leaves it alone.
	prefetchStreamGrace = 5 * time.Minute
)

// SubtitlePrefetchProgress is what the settings page shows.
type SubtitlePrefetchProgress struct {
	Enabled bool `json:"enabled"`
	Running bool `json:"running"`
	// Files is how many of the library's videos have been looked at, of Total.
	Files int `json:"files"`
	Total int `json:"total"`
	// Tracks is how many subtitle tracks the pass has put on disk.
	Tracks  int    `json:"tracks"`
	Current string `json:"current"`
	Failed  int    `json:"failed"`
	Error   string `json:"error"`
}

var prefetch struct {
	sync.Mutex
	once sync.Once
	p    SubtitlePrefetchProgress
}

func prefetchUpdate(fn func(p *SubtitlePrefetchProgress)) {
	prefetch.Lock()
	fn(&prefetch.p)
	prefetch.Unlock()
}

// GetSubtitlePrefetchProgress reports the library prefetch.
func GetSubtitlePrefetchProgress(c *gin.Context) {
	prefetch.Lock()
	p := prefetch.p
	prefetch.Unlock()
	p.Enabled = setting.GetBool(conf.SubtitlePrefetchLibrary)
	common.SuccessResp(c, p)
}

// StartSubtitlePrefetch starts the worker that follows the setting: a pass over
// the library when it is turned on, and again an hour after each pass, until it
// is turned off, which stops the pass in progress.
func StartSubtitlePrefetch() {
	prefetch.once.Do(func() { go prefetchLoop() })
}

func prefetchLoop() {
	var lastPass time.Time
	for {
		time.Sleep(prefetchPoll)
		if !setting.GetBool(conf.SubtitlePrefetchLibrary) {
			lastPass = time.Time{}
			continue
		}
		if !lastPass.IsZero() && time.Since(lastPass) < prefetchRepeat {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			defer cancel()
			for ctx.Err() == nil {
				time.Sleep(prefetchPoll)
				if !setting.GetBool(conf.SubtitlePrefetchLibrary) {
					return
				}
			}
		}()
		runPrefetchPass(ctx)
		cancel()
		lastPass = time.Now()
	}
}

// libraryVideoPaths lists the video files of the media library.
func libraryVideoPaths() ([]string, error) {
	items, err := db.ListVideoItemsUnder("/")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var paths []string
	for i := range items {
		for _, t := range media.StrmTargets(&items[i]) {
			if t.RawPath != "" && !seen[t.RawPath] {
				seen[t.RawPath] = true
				paths = append(paths, t.RawPath)
			}
		}
	}
	return paths, nil
}

// openPrefetchTarget resolves a library path for the prefetch: the storage, the
// file, and the tracks its provider already supplies.
func openPrefetchTarget(ctx context.Context, path string) (*subextract.Target, error) {
	storage, err := fs.GetStorage(path, &fs.GetStoragesArgs{})
	if err != nil {
		return nil, err
	}
	if !wantsExtraction(storage, stdpath.Base(path)) {
		return nil, nil
	}
	obj, err := fs.Get(ctx, path, &fs.GetArgs{NoLog: true})
	if err != nil || obj.IsDir() {
		return nil, err
	}
	file := extractFile(storage, path, obj)
	streaming := func() bool { return stream.ServedWithin(path, prefetchStreamGrace) }
	vsp, ok := storage.(driver.VideoSubtitleProvider)
	if !ok {
		return &subextract.Target{File: file, Streaming: streaming}, nil
	}
	return &subextract.Target{File: file, Streaming: streaming, Cover: func(ctx context.Context) (func(subextract.TrackInfo) bool, error) {
		provided, err := vsp.VideoSubtitle(ctx, obj)
		if err != nil {
			return nil, err
		}
		return func(t subextract.TrackInfo) bool { return trackCovered(t, provided) }, nil
	}}, nil
}

var (
	prefetcherOnce sync.Once
	prefetcher     *subextract.Prefetcher
)

func runPrefetchPass(ctx context.Context) {
	paths, err := libraryVideoPaths()
	prefetchUpdate(func(p *SubtitlePrefetchProgress) {
		*p = SubtitlePrefetchProgress{Running: err == nil, Total: len(paths)}
		if err != nil {
			p.Error = err.Error()
		}
	})
	if err != nil {
		log.Warnf("subtitle prefetch: listing the library: %v", err)
		return
	}
	prefetcherOnce.Do(func() {
		prefetcher = subextract.NewPrefetcher(SubtitleExtractor(), openPrefetchTarget)
	})
	prefetcher.Run(ctx, paths, func(st subextract.PrefetchState) {
		prefetchUpdate(func(p *SubtitlePrefetchProgress) {
			p.Files, p.Tracks, p.Failed, p.Current = st.Files, st.Tracks, st.Failed, st.Current
			p.Running = st.Current != "" || st.Files < len(paths)
			if st.Err != nil {
				p.Error = st.Err.Error()
			}
		})
	})
	prefetchUpdate(func(p *SubtitlePrefetchProgress) { p.Running, p.Current = false, "" })
}

// missingTracks are the orders of the tracks that can be extracted and that none
// of the provider's files stands in for.
func missingTracks(tracks []subextract.TrackInfo, provided []driver.VideoSubtitleInfo) []int {
	var out []int
	for _, t := range tracks {
		if t.Supported && !trackCovered(t, provided) {
			out = append(out, t.Order)
		}
	}
	return out
}
