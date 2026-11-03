package media

import (
	"context"
	"fmt"
	stdpath "path"

	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// maxCollectionDepth is how many levels below a scan target a collection
// ("昆汀合集") is opened to find the works inside it.
const maxCollectionDepth = 3

// listEntries lists one directory for the identification rules. It refreshes
// nothing: the scan has just walked the directory.
func listEntries(ctx context.Context, p string) ([]identify.Entry, error) {
	objs, err := scanList(ctx, p, &fs.ListArgs{NoLog: true})
	if err != nil {
		return nil, err
	}
	out := make([]identify.Entry, 0, len(objs))
	for _, o := range objs {
		out = append(out, identify.Entry{Name: o.GetName(), IsDir: o.IsDir(), Size: o.GetSize()})
	}
	return out, nil
}

// identifyScanItem decides what a video scan target is (movie, tv, anime,
// collection or ignore) from its name and, for a folder, its content, and
// writes the verdict into item. It returns the folder's entries (nil for a
// file, or when the folder could not be listed) and the verdict.
//
// A few ambiguous verdicts look one level further down (numbered directories,
// a wrapper directory); nothing else is listed. Nothing here searches a
// provider: the title and year it writes are the query a later scrape starts
// from, and a scrape replaces them.
func identifyScanItem(ctx context.Context, vfsPath string, item *model.MediaItem) ([]identify.Entry, identify.Result) {
	entries, r, _ := identifyScanItemChecked(ctx, vfsPath, item)
	return entries, r
}

// identifyScanItemChecked is identifyScanItem that also reports the first
// listing that failed (the folder's own or one a rule looked into): the verdict
// then rests on the name alone and must not replace a better one.
func identifyScanItemChecked(ctx context.Context, vfsPath string, item *model.MediaItem) ([]identify.Entry, identify.Result, error) {
	var first error
	note := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	var entries []identify.Entry
	if item.IsFolder {
		var err error
		entries, err = listEntries(ctx, vfsPath)
		note(err)
	}
	peek := func(name string) ([]identify.Entry, error) {
		es, err := listEntries(ctx, stdpath.Join(vfsPath, name))
		note(err)
		return es, err
	}
	r := identify.ClassifyWith(item.FileName, item.IsFolder, entries, peek)
	applyIdentify(item, r)
	if !item.IsFolder && model.IsEpisodicVideo(r.Kind) {
		if s, e, ok := identify.EpisodeOf(item.FileName); ok {
			if s > 0 {
				item.Season = s
			}
			item.Episode = e
		}
	}
	return entries, r, first
}

// applyIdentify copies an identification into a scanned item.
func applyIdentify(item *model.MediaItem, r identify.Result) {
	item.VideoType = r.Kind
	item.Source = model.MediaSourceRule
	item.Confidence = r.Confidence
	item.Hidden = item.Hidden || r.Kind == model.VideoTypeIgnore
	if r.Title != "" {
		item.ScrapedName = r.Title
	}
	item.OriginalTitle = r.English
	if r.Year > 0 {
		item.ReleaseDate = fmt.Sprintf("%04d-01-01", r.Year)
	}
	if r.Season > 0 {
		item.Season = r.Season
	}
}

// settleVerdict applies what a scan knows beyond the rules of one folder.
// A member of a collection with no title of its own ("2", 纯英) takes the
// collection's. A folder the rules took for several works, but that a scraper or
// a person has since made one show, stays that show (decided gives the stored
// kind): the scan then reads its episodes and no longer opens it into member
// rows, which the prune removes.
func settleVerdict(item *model.MediaItem, r *identify.Result, member bool, decided func(folder, name string) string) {
	if member && r.InheritParentTitle(item.FileName, !item.IsFolder, stdpath.Base(item.FolderPath)) {
		applyIdentify(item, *r)
	}
	if item.IsFolder && r.MaybeShow() {
		if kind := decided(item.FolderPath, item.FileName); kind != "" {
			r.Kind = kind
			applyIdentify(item, *r)
		}
	}
}

// collectionChildren lists what to scan inside a collection folder: its video
// files and its directories, minus the directories that hold extras.
func collectionChildren(parent string, entries []identify.Entry) []string {
	var out []string
	for _, e := range entries {
		switch {
		case e.IsDir && identify.IsExtraDir(e.Name):
		case e.IsDir || identify.IsVideo(e.Name):
			out = append(out, stdpath.Join(parent, e.Name))
		}
	}
	return out
}

// scanEpisodes finds the episode files of a show folder (its own videos and
// those in its season directories).
func scanEpisodes(ctx context.Context, folder string) ([]model.MediaEpisode, error) {
	files, err := identify.EpisodeFiles(func(rel string) ([]identify.Entry, error) {
		return listEntries(ctx, stdpath.Join(folder, rel))
	})
	if err != nil {
		return nil, err
	}
	eps := make([]model.MediaEpisode, 0, len(files))
	for _, f := range files {
		eps = append(eps, model.MediaEpisode{FileName: f.Rel, Season: f.Season, Episode: f.Episode})
	}
	return eps, nil
}
