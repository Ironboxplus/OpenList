package apply

import (
	"context"
	"encoding/json"
	"path"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/nfo"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// NFOPutter writes one small file into a directory of the library.
type NFOPutter func(ctx context.Context, dir, name string, data []byte) error

// ExportResult is what ExportNFO decided (and, with apply, did) for one work.
type ExportResult struct {
	Path   string `json:"path"`
	File   string `json:"file,omitempty"`   // the NFO that is (or would be) written
	Action string `json:"action"`           // "write", "written", "skip" or "error"
	Reason string `json:"reason,omitempty"` // why it was skipped or failed
}

// ExportNFO writes an NFO next to every work in items that a person fixed (locked
// or picked by hand) and that has a TMDB or IMDb id, so that another player or a
// later rebuild of the library reads the same match back (T6a reads it).
//
// It never overwrites: a work with any NFO candidate next to it is skipped. It
// writes nothing unless apply is set; without it the results say what it would
// write ("write"). Uploads are strictly sequential, one at a time, because the
// directories may sit on a drive that must not be written concurrently (115).
func ExportNFO(ctx context.Context, l Lister, put NFOPutter, items []model.MediaItem, apply bool) []ExportResult {
	out := make([]ExportResult, 0, len(items))
	for i := range items {
		it := &items[i]
		p := path.Join("/", it.FolderPath, it.FileName)
		r := ExportResult{Path: p, Action: "skip"}
		if err := ctx.Err(); err != nil {
			r.Action, r.Reason = "error", err.Error()
			out = append(out, r)
			break
		}
		info, why := nfoInfoOf(it)
		if why != "" {
			r.Reason = why
			out = append(out, r)
			continue
		}
		dir := path.Join("/", it.FolderPath)
		if it.IsFolder {
			dir = p
		}
		entries, err := l.List(ctx, dir)
		if err != nil {
			r.Action, r.Reason = "error", "list "+dir+": "+err.Error()
			out = append(out, r)
			continue
		}
		names := make([]string, 0, len(entries))
		video, best := "", int64(-1)
		for _, e := range entries {
			names = append(names, e.Name)
			if it.IsFolder && !e.IsDir && identify.IsVideo(e.Name) && e.Size > best {
				video, best = e.Name, e.Size
			}
		}
		if have := nfo.Find(names, nfo.Candidates(it.IsFolder, it.FileName, video, info.Kind)); have != "" {
			r.File, r.Reason = have, "an NFO is already there"
			out = append(out, r)
			continue
		}
		r.File = nfo.FileName(it.IsFolder, info.Kind, it.FileName)
		data, err := nfo.Render(info)
		if err != nil {
			r.Action, r.Reason = "error", err.Error()
			out = append(out, r)
			continue
		}
		r.Action = "write"
		if apply {
			if err := put(ctx, dir, r.File, data); err != nil {
				r.Action, r.Reason = "error", err.Error()
			} else {
				r.Action = "written"
			}
		}
		out = append(out, r)
	}
	return out
}

// nfoInfoOf is the NFO content of a fixed work, or the reason it has none.
func nfoInfoOf(it *model.MediaItem) (nfo.Info, string) {
	switch {
	case it.Hidden:
		return nfo.Info{}, "hidden (folded into another work)"
	case !it.Locked && it.Source != model.MediaSourceManual:
		return nfo.Info{}, "not locked and not picked by hand"
	case it.VideoType == model.VideoTypeCollection || it.VideoType == model.VideoTypeIgnore:
		return nfo.Info{}, "not a movie or a show"
	}
	var ids map[string]string
	if it.ExternalIDs != "" {
		_ = json.Unmarshal([]byte(it.ExternalIDs), &ids)
	}
	info := nfo.Info{Title: it.ScrapedName, OriginalTitle: it.OriginalTitle, Year: nfo.YearOf(it.ReleaseDate), IMDb: ids["imdb"]}
	if typ, id, ok := strings.Cut(ids["tmdb"], "/"); ok {
		info.Kind, info.TMDB = map[string]string{"movie": "movie", "tv": "tv"}[typ], id
	}
	if info.Kind == "" {
		// no typed TMDB id: the kind of the work says what the NFO is
		info.Kind = "tv"
		if it.VideoType == model.VideoTypeMovie {
			info.Kind = "movie"
		}
	}
	if !info.HasID() {
		return nfo.Info{}, "no TMDB or IMDb id"
	}
	return info, ""
}
