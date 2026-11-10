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
	mgr := SubtitleExtractor()
	for _, path := range paths {
		for mgr.Busy() && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(prefetchBusyWait):
			}
		}
		if ctx.Err() != nil {
			break
		}
		prefetchUpdate(func(p *SubtitlePrefetchProgress) { p.Current = path })
		n, err := prefetchFile(ctx, path)
		failed := err != nil && ctx.Err() == nil
		if failed {
			log.Debugf("subtitle prefetch: %s: %v", path, err)
		}
		prefetchUpdate(func(p *SubtitlePrefetchProgress) {
			p.Files++
			p.Tracks += n
			if failed {
				p.Failed++
				p.Error = err.Error()
			}
		})
	}
	prefetchUpdate(func(p *SubtitlePrefetchProgress) { p.Running, p.Current = false, "" })
}

// prefetchFile extracts the tracks of one video that no provider file covers
// and returns how many it put on disk.
func prefetchFile(ctx context.Context, path string) (int, error) {
	storage, err := fs.GetStorage(path, &fs.GetStoragesArgs{})
	if err != nil {
		return 0, err
	}
	if !wantsExtraction(storage, stdpath.Base(path)) {
		return 0, nil
	}
	obj, err := fs.Get(ctx, path, &fs.GetArgs{NoLog: true})
	if err != nil || obj.IsDir() {
		return 0, err
	}
	mgr := SubtitleExtractor()
	file := extractFile(storage, path, obj)
	tracks, err := mgr.Tracks(ctx, file)
	if err != nil {
		return 0, err
	}
	var provided []driver.VideoSubtitleInfo
	if vsp, ok := storage.(driver.VideoSubtitleProvider); ok {
		if provided, err = vsp.VideoSubtitle(ctx, obj); err != nil {
			return 0, err
		}
	}
	var todo []int
	for _, o := range missingTracks(tracks, provided) {
		if !mgr.Cached(file, o) {
			todo = append(todo, o)
		}
	}
	if err := mgr.Fill(ctx, file, todo); err != nil {
		return 0, err
	}
	return len(todo), nil
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
