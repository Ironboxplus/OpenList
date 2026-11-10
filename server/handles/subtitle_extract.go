package handles

import (
	"context"
	"fmt"
	"net/url"
	stdpath "path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/media/subextract"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/internal/transcode"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// subtitleSourceExtracted marks a track OpenList read out of the video itself
// (driver.VideoSubtitleInfo.Source), as against one the provider extracted.
const subtitleSourceExtracted = "extracted"

// ExtractHeader tells how far an extracted track has got: "partial; done=0.42"
// while it is being read, "complete" once it is whole (and cached).
const ExtractHeader = "X-OpenList-Extract"

// extractFirstWait is how long a request waits for the first cues of a track
// that has only just started being read.
const extractFirstWait = 4 * time.Second

var (
	subtitleManagerOnce sync.Once
	subtitleManager     *subextract.Manager
)

// SubtitleExtractor is the process's subtitle extraction manager.
func SubtitleExtractor() *subextract.Manager {
	subtitleManagerOnce.Do(func() {
		subtitleManager = subextract.NewManager(subextract.Config{
			Dir: filepath.Join(conf.DataDir, "subtitle-cache"),
			MaxCacheBytes: func() int64 {
				return int64(setting.GetInt(conf.SubtitleCacheMaxMB, 512)) << 20
			},
			MaxFiles: func() int { return setting.GetInt(conf.SubtitleExtractConcurrency, 2) },
		})
	})
	return subtitleManager
}

// extractContainers are the extensions whose containers carry subtitle tracks
// that can be read with ranged reads.
var extractContainers = map[string]bool{
	".mkv": true, ".webm": true, ".mka": true, ".mp4": true, ".m4v": true, ".mov": true,
}

// extractFile is the file at path, as the extraction manager wants it: keyed by
// revision, opened through fs.Link like any other read of it.
func extractFile(storage driver.Driver, path string, obj model.Obj) subextract.File {
	mount := ""
	if s := storage.GetStorage(); s != nil {
		mount = s.MountPath
	}
	return subextract.File{
		Key: fmt.Sprintf("%s|%s|%d|%d", mount, path, obj.GetSize(), obj.ModTime().UnixNano()),
		Open: func(ctx context.Context) (subextract.Source, func(), error) {
			link, _, err := fs.Link(ctx, path, model.LinkArgs{})
			if err != nil {
				return nil, nil, err
			}
			src, err := subextract.NewLinkSource(link, obj.GetSize())
			if err != nil {
				_ = link.Close()
				return nil, nil, err
			}
			return src, func() { _ = link.Close() }, nil
		},
	}
}

// wantsExtraction is true when the video has a source other than the original
// to be played on: a driver with its own online-play sources, or OpenList's
// transcoder. With only the original, its embedded tracks already work and
// nothing is probed or read.
func wantsExtraction(storage any, name string) bool {
	if !extractContainers[strings.ToLower(stdpath.Ext(name))] {
		return false
	}
	if _, ok := storage.(driver.VideoPlayer); ok {
		return true
	}
	return transcode.IsEnabled()
}

// extractedSubtitles lists the container's subtitle tracks that no other entry
// of subs stands in for, as files OpenList will extract on request. It also
// starts reading them in the background, so that they are ready (or on their
// way) by the time one is picked.
func extractedSubtitles(c *gin.Context, storage driver.Driver, reqPath string, obj model.Obj, subs []driver.VideoSubtitleInfo) []driver.VideoSubtitleInfo {
	if !wantsExtraction(storage, obj.GetName()) {
		return nil
	}
	mgr := SubtitleExtractor()
	file := extractFile(storage, reqPath, obj)
	tracks, err := mgr.Tracks(c.Request.Context(), file)
	if err != nil {
		log.Debugf("subtitle extract: probing %s: %v", reqPath, err)
		return nil
	}
	apiURL := common.GetApiUrl(c)
	s := sign.Sign(reqPath)
	var out []driver.VideoSubtitleInfo
	orders := missingTracks(tracks, subs)
	for _, order := range orders {
		t := tracks[order]
		q := url.Values{"path": {reqPath}, "track": {strconv.Itoa(order)}, "sign": {s}}
		out = append(out, driver.VideoSubtitleInfo{
			Language:      t.Language,
			Title:         t.Title,
			URL:           apiURL + "/api/fs/subtitle_extract?" + q.Encode(),
			Type:          t.Format,
			Source:        subtitleSourceExtracted,
			EmbeddedTitle: t.Title,
			EmbeddedOrder: &order,
		})
	}
	mgr.Prepare(file, orders)
	return out
}

// trackCovered is the player's own rule for whether an extracted file stands in
// for an in-band track (movi SourceSelection confirms()): by the track's order
// in the container, and the entry is believed when the titles agree or, if
// either has none, the languages do.
func trackCovered(t subextract.TrackInfo, subs []driver.VideoSubtitleInfo) bool {
	for _, s := range subs {
		if s.EmbeddedOrder != nil {
			if *s.EmbeddedOrder == t.Order && embeddedAgrees(t, s) {
				return true
			}
			continue
		}
		if s.EmbeddedTitle != "" && t.Title != "" && sameText(s.EmbeddedTitle, t.Title) {
			return true
		}
	}
	return false
}

func embeddedAgrees(t subextract.TrackInfo, s driver.VideoSubtitleInfo) bool {
	if s.EmbeddedTitle != "" && t.Title != "" {
		return sameText(s.EmbeddedTitle, t.Title)
	}
	return subextract.SameLanguage(t.Language, s.Language)
}

func sameText(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// SubtitleExtract serves one subtitle track extracted from a video, as a file
// in the track's own format. It is what the <track> element or the player
// fetches, so it carries no login: the signed URL (the same signature /d takes)
// is the authorisation. While the track is still being read the response is the
// part known so far and says so in ExtractHeader.
func SubtitleExtract(c *gin.Context) {
	path := utils.FixAndCleanPath(c.Query("path"))
	if err := sign.Verify(path, strings.TrimSuffix(c.Query("sign"), "/")); err != nil {
		common.ErrorStrResp(c, "sign is invalid", 401)
		return
	}
	order, err := strconv.Atoi(c.Query("track"))
	if err != nil || order < 0 {
		common.ErrorStrResp(c, "track is invalid", 400)
		return
	}
	// Where reading starts: the playback position when the player says it, and
	// otherwise wherever it already is (a request without one must not pull a
	// read that follows a viewer back to the start).
	atMs := int64(-1)
	if at, err := strconv.ParseFloat(c.Query("at"), 64); err == nil && at >= 0 {
		atMs = int64(at * 1000)
	}
	storage, err := fs.GetStorage(path, &fs.GetStoragesArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	obj, err := fs.Get(c.Request.Context(), path, &fs.GetArgs{NoLog: true})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if obj.IsDir() || !extractContainers[strings.ToLower(stdpath.Ext(obj.GetName()))] {
		common.ErrorStrResp(c, "not a video container", 400)
		return
	}
	res, err := SubtitleExtractor().Get(c.Request.Context(), extractFile(storage, path, obj), order, atMs, extractFirstWait)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if res.Partial {
		c.Header(ExtractHeader, fmt.Sprintf("partial; done=%.2f", res.Done))
		c.Header("Cache-Control", "no-store")
	} else {
		c.Header(ExtractHeader, "complete")
		c.Header("Cache-Control", "private, max-age=600")
	}
	c.Header("Access-Control-Expose-Headers", ExtractHeader)
	ctype := "text/plain; charset=utf-8"
	if res.Format == subextract.FormatSUP {
		ctype = "application/octet-stream"
	}
	c.Data(200, ctype, res.Data)
}
