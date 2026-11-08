package media

import (
	"context"
	stdpath "path"
	"sort"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	log "github.com/sirupsen/logrus"
)

// sidecarExts are the subtitle files recorded next to a video.
var sidecarExts = map[string]bool{".ass": true, ".srt": true, ".vtt": true, ".sub": true, ".ssa": true, ".sup": true}

// SidecarNames returns the names in names that are subtitles of video: they
// start with the video's name without its extension followed by '.', the rule
// Jellyfin reads external files by (MediaInfoResolver.GetExternalFiles).
func SidecarNames(video string, names []string) []string {
	prefix := strings.TrimSuffix(video, stdpath.Ext(video)) + "."
	var out []string
	for _, n := range names {
		if n != video && strings.HasPrefix(n, prefix) && sidecarExts[strings.ToLower(stdpath.Ext(n))] {
			out = append(out, n)
		}
	}
	return out
}

// itemBaseDir is the directory the relative paths of a work's episodes and
// sidecars start from.
func itemBaseDir(item *model.MediaItem) string {
	if item.IsFolder {
		return db.MediaItemPath(item)
	}
	return MediaItemFolder(item)
}

// recordSidecars replaces the sidecar rows of a saved item with the subtitles
// found next to its videos (paths relative to the work, see model.MediaSidecar).
// Each directory is listed once, from what the scan has just walked. When a
// listing fails the old rows stay: a failed listing proves nothing.
func recordSidecars(ctx context.Context, item *model.MediaItem, videos []string, list listFunc) {
	if item == nil || item.ID == 0 {
		return
	}
	base := itemBaseDir(item)
	byDir := map[string]map[string]int64{} // directory relative to base -> file names in it, with sizes
	var rows []model.MediaSidecar
	for _, v := range videos {
		v = stdpath.Clean(v)
		dir := stdpath.Dir(v)
		files, ok := byDir[dir]
		if !ok {
			entries, err := list(ctx, stdpath.Join(base, dir))
			if err != nil {
				log.Warnf("media sidecars: list %s: %v", stdpath.Join(base, dir), err)
				return
			}
			files = make(map[string]int64, len(entries))
			for _, e := range entries {
				if !e.IsDir {
					files[e.Name] = e.Size
				}
			}
			byDir[dir] = files
		}
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, s := range SidecarNames(stdpath.Base(v), names) {
			p := stdpath.Join(dir, s)
			if len(v) > model.MediaIndexColumnMax || len(p) > model.MediaIndexColumnMax {
				continue
			}
			rows = append(rows, model.MediaSidecar{Video: v, Path: p, Size: files[s]})
		}
	}
	if err := db.ReplaceMediaSidecars(item.ID, rows); err != nil {
		log.Warnf("media sidecars: save [%s]: %v", db.MediaItemPath(item), err)
	}
}

// episodeFiles are the file names of the episode rows.
func episodeFiles(eps []model.MediaEpisode) []string {
	out := make([]string, 0, len(eps))
	for _, e := range eps {
		out = append(out, e.FileName)
	}
	return out
}
