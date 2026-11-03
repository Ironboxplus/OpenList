package media

import (
	"context"
	"encoding/json"
	stdpath "path"
	"sort"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	log "github.com/sirupsen/logrus"
)

// listFunc lists one directory for the consolidation.
type listFunc func(ctx context.Context, p string) ([]identify.Entry, error)

// ConsolidateVideoFolders folds the releases of one film that sit side by side
// into one work. Works in the same folder that a scrape matched to the same
// TMDB film are the versions of it (毒液: 60帧 and 120帧): the first stays, the
// others are hidden and their files become the versions of the first, as the
// files of one folder do. The same model as Radarr's: a film is its TMDB id and
// a film has files; Jellyfin only does this inside one folder
// (Emby.Naming VideoListResolver.GetVideosGroupedByVersion), which is not where
// release folders are.
//
// It is safe to repeat: a rescan overwrites the survivor's episode list and the
// next call rebuilds it. A locked or hand-made work is never folded. The
// listings are sequential, one per work of a group.
func ConsolidateVideoFolders(ctx context.Context, folders []string) {
	seen := map[string]bool{}
	for _, f := range folders {
		if seen[f] {
			continue
		}
		seen[f] = true
		if err := consolidateFolder(ctx, f, listEntries); err != nil {
			log.Warnf("media consolidate [%s]: %v", f, err)
		}
	}
}

func tmdbOf(it *model.MediaItem) string {
	if it.ExternalIDs == "" {
		return ""
	}
	var ids map[string]string
	if json.Unmarshal([]byte(it.ExternalIDs), &ids) != nil {
		return ""
	}
	return ids["tmdb"]
}

func consolidateFolder(ctx context.Context, folder string, list listFunc) error {
	items, err := db.ListVideoItemsInFolder(folder)
	if err != nil {
		return err
	}
	groups := map[string][]*model.MediaItem{}
	for i := range items {
		it := &items[i]
		id := tmdbOf(it)
		if !(strings.HasPrefix(id, "movie/") || strings.HasPrefix(id, "tv/")) || it.Locked || it.Source == model.MediaSourceManual {
			continue
		}
		groups[id] = append(groups[id], it)
	}
	ids := make([]string, 0, len(groups))
	for id, g := range groups {
		if len(g) > 1 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		fold := foldFilm
		if strings.HasPrefix(id, "tv/") {
			fold = foldShowReleases
		}
		if err := fold(ctx, groups[id], list); err != nil {
			return err
		}
	}
	// the works of this folder may be the seasons of the show that the folder is
	return foldSeasonsIntoCollection(ctx, folder, items, list)
}

// foldFilm folds the works of one film into the first visible one.
func foldFilm(ctx context.Context, group []*model.MediaItem, list listFunc) error {
	var survivor *model.MediaItem
	for _, it := range group {
		if !it.Hidden {
			survivor = it
			break
		}
	}
	if survivor == nil {
		return nil // all hidden by hand: not ours to undo
	}
	var files []identify.Entry // Name is the path relative to the survivor
	var others []uint
	for _, it := range group {
		mine := it == survivor
		prefix := ""
		if !mine {
			prefix = "../" + it.FileName + "/"
			others = append(others, it.ID)
		}
		if !it.IsFolder {
			// a loose file is its own version
			name := it.FileName
			if !mine {
				name = "../" + it.FileName
			}
			files = append(files, identify.Entry{Name: name, Size: it.FileSize})
			continue
		}
		entries, err := list(ctx, db.MediaItemPath(it))
		if err != nil {
			return err
		}
		peek := func(name string) ([]identify.Entry, error) { return list(ctx, stdpath.Join(db.MediaItemPath(it), name)) }
		var videos []identify.Entry
		for _, e := range identify.UnwrapEntries(entries, peek) {
			if !e.IsDir && identify.IsVideo(e.Name) {
				videos = append(videos, identify.Entry{Name: prefix + e.Rel(), Size: e.Size})
			}
		}
		files = append(files, identify.MainVideos(videos)...)
	}
	if len(files) < 2 {
		return nil
	}
	if err := db.UpdateMediaItemEpisodes(survivor.ID, model.VersionsJSON(identify.VersionOrder(files))); err != nil {
		return err
	}
	return db.FoldMediaItems(survivor.ID, others)
}

// episodesOf lists the episode files of a show folder through list; prefix is put
// in front of every file name (the way back to the folder from the work that
// holds the episode).
func episodesOf(ctx context.Context, list listFunc, folder, prefix string) ([]model.MediaEpisode, error) {
	files, err := identify.EpisodeFiles(func(rel string) ([]identify.Entry, error) {
		return list(ctx, stdpath.Join(folder, rel))
	})
	if err != nil {
		return nil, err
	}
	eps := make([]model.MediaEpisode, 0, len(files))
	for _, f := range files {
		eps = append(eps, model.MediaEpisode{FileName: prefix + f.Rel, Season: f.Season, Episode: f.Episode, Version: f.Version})
	}
	return eps, nil
}

// foldShowReleases folds works that are releases of one show (the same TMDB tv
// id, side by side: 回复术士 as VCB and as H-Enc, a BDBOX 1&2 beside a BDBOX3)
// into the first visible one: the episodes of the others that it lacks are added
// and the ones it has become versions of its episodes, as the release folders of
// one show do (Jellyfin VideoListResolver.GetEpisodesGroupedByVersion). Names
// that are one title and a number are seasons of a show, not releases, and are
// not folded here.
func foldShowReleases(ctx context.Context, group []*model.MediaItem, list listFunc) error {
	var survivor *model.MediaItem
	var names []identify.Entry
	for _, it := range group {
		names = append(names, identify.Entry{Name: it.FileName, IsDir: true})
		if survivor == nil && !it.Hidden {
			survivor = it
		}
	}
	if survivor == nil || !survivor.IsFolder || !identify.SameTitleSiblings(names) {
		return nil
	}
	eps, err := episodesOf(ctx, list, db.MediaItemPath(survivor), "")
	if err != nil {
		return err
	}
	taken := map[[2]int]bool{}
	for _, e := range eps {
		taken[[2]int{e.Season, e.Episode}] = true
	}
	var others []uint
	for _, it := range group {
		if it == survivor {
			continue
		}
		others = append(others, it.ID)
		if !it.IsFolder {
			continue
		}
		more, err := episodesOf(ctx, list, db.MediaItemPath(it), "../"+it.FileName+"/")
		if err != nil {
			return err
		}
		for _, e := range more {
			k := [2]int{e.Season, e.Episode}
			e.Version = e.Version || taken[k]
			taken[k] = true
			eps = append(eps, e)
		}
	}
	if len(others) == 0 {
		return nil
	}
	merged, err := db.SyncMediaEpisodes(survivor.ID, eps)
	if err != nil {
		return err
	}
	if err := db.UpdateMediaItemEpisodes(survivor.ID, model.EpisodesJSON(merged)); err != nil {
		return err
	}
	return db.FoldMediaItems(survivor.ID, others)
}

// foldSeasonsIntoCollection turns a folder the rules took for a collection into
// the show it is, when a scrape matched all its works to the same TMDB tv id and
// their names are one title and a number (Log Horizon, Log Horizon 2): the
// folder takes the matched fields of the first work, its episodes are read
// season by season, and the works are folded into it. Without a number in the
// names the seasons cannot be told apart from copies of one season; that is
// left to a person.
func foldSeasonsIntoCollection(ctx context.Context, folder string, members []model.MediaItem, list listFunc) error {
	parent, err := db.GetVideoItemByPath(folder)
	if err != nil || parent == nil || parent.VideoType != model.MediaKindCollection || parent.Locked || parent.Source == model.MediaSourceManual {
		return err
	}
	var seasons []*model.MediaItem
	id := ""
	var names []identify.Entry
	for i := range members {
		it := &members[i]
		if it.Hidden {
			continue
		}
		if !it.IsFolder || !model.IsEpisodicVideo(it.VideoType) || !strings.HasPrefix(tmdbOf(it), "tv/") || (id != "" && tmdbOf(it) != id) {
			return nil
		}
		id = tmdbOf(it)
		seasons = append(seasons, it)
		names = append(names, identify.Entry{Name: it.FileName, IsDir: true})
	}
	if len(seasons) < 2 || identify.SiblingSeasons(names) == nil {
		return nil
	}
	first := seasons[0]
	parent.VideoType, parent.Source, parent.Confidence = first.VideoType, model.MediaSourceScraper, first.Confidence
	parent.ScrapedName, parent.OriginalTitle, parent.ExternalIDs, parent.ExternalID = first.ScrapedName, first.OriginalTitle, first.ExternalIDs, first.ExternalID
	parent.Cover, parent.Backdrop, parent.Plot, parent.Description = first.Cover, first.Backdrop, first.Plot, first.Description
	parent.Genre, parent.Rating, parent.ReleaseDate, parent.ScrapedAt = first.Genre, first.Rating, first.ReleaseDate, first.ScrapedAt
	parent.IdentifyVersion, parent.Candidates = first.IdentifyVersion, ""
	eps, err := episodesOf(ctx, list, db.MediaItemPath(parent), "")
	if err != nil {
		return err
	}
	parent.Episodes = model.EpisodesJSON(eps)
	ratings, err := db.ListMediaRatings(first.ID)
	if err != nil {
		return err
	}
	if err := db.SaveVideoItem(parent, ratings[first.ID], eps); err != nil {
		return err
	}
	ids := make([]uint, 0, len(seasons))
	for _, s := range seasons {
		ids = append(ids, s.ID)
	}
	return db.FoldMediaItems(parent.ID, ids)
}
