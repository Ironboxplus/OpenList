package handles

import (
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/media/store"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// childPosterCount is how many posters a collection shows as its mosaic.
const childPosterCount = 4

// MediaItemView is an item as the public media APIs return it: the stored
// fields (what the upstream API always returned) plus what the poster wall
// needs, so that no second endpoint is required.
type MediaItemView struct {
	model.MediaItem
	// Path is where the item lives, as the user sees paths (relative to their
	// base path).
	Path        string         `json:"path"`
	Ratings     map[string]any `json:"ratings"`
	CoverURL    string         `json:"cover_url,omitempty"`
	BackdropURL string         `json:"backdrop_url,omitempty"`
	// ChildPosters is set for collections only: up to 4 cover urls of what is
	// inside, by release year then title; what the user may not read is skipped.
	ChildPosters *[]string `json:"child_posters,omitempty"`
	// EpisodeList is set on the detail only.
	EpisodeList *[]MediaEpisodeView `json:"episode_list,omitempty"`
	// Versions is how many versions of the film the folder holds (set when more
	// than one): clients show a version picker, not an episode list, and the
	// entries of episode_list are marked version.
	Versions int `json:"versions,omitempty"`
}

// MediaEpisodeView is one episode of a show.
type MediaEpisodeView struct {
	FileName string `json:"file_name"`
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Title    string `json:"title"`
	Overview string `json:"overview"`
	StillURL string `json:"still_url,omitempty"`
	// Version marks a version of the film, not an episode of a show.
	Version bool `json:"version,omitempty"`
}

// mediaViewer builds the views of one response for one user.
type mediaViewer struct {
	user    *model.User
	rel     func(string) string
	ratings map[uint][]model.MediaRating
}

// newMediaViewer prepares the views of items (their ratings are read in one
// query).
func newMediaViewer(user *model.User, items []model.MediaItem) (*mediaViewer, error) {
	ids := make([]uint, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}
	rs, err := db.ListMediaRatings(ids...)
	if err != nil {
		return nil, err
	}
	v := &mediaViewer{user: user, ratings: rs, rel: identityPath}
	if user != nil {
		v.rel = userRelPath(user)
	}
	return v, nil
}

// view is the compact view: no episode list. Candidates are for admins only.
func (v *mediaViewer) view(it *model.MediaItem) MediaItemView {
	out := MediaItemView{
		MediaItem: *it, Path: v.rel(db.MediaItemPath(it)),
		Ratings:  store.RatingsToMap(v.ratings[it.ID]),
		CoverURL: mediaCoverURL(it.Cover), BackdropURL: mediaCoverURL(it.Backdrop),
	}
	if v.user == nil || !v.user.IsAdmin() {
		out.Candidates = ""
	}
	if it.MediaType == model.MediaTypeVideo && it.VideoType == model.MediaKindCollection {
		posters := v.childPosters(it)
		out.ChildPosters = &posters
	}
	if vs := model.ItemVersions(it.Episodes); len(vs) > 1 {
		out.Versions = len(vs)
	}
	return out
}

// detail is view plus the episode list.
func (v *mediaViewer) detail(it *model.MediaItem) (MediaItemView, error) {
	out := v.view(it)
	if vs := model.ItemVersions(it.Episodes); len(vs) > 1 {
		// a film kept in several versions: the entries are the files, not episodes
		list := make([]MediaEpisodeView, 0, len(vs))
		for _, e := range vs {
			list = append(list, MediaEpisodeView{FileName: e.FileName, Episode: e.Index, Title: e.Title, Version: true})
		}
		out.EpisodeList = &list
		return out, nil
	}
	eps, err := db.ListMediaEpisodes(it.ID)
	if err != nil {
		return out, err
	}
	list := make([]MediaEpisodeView, 0, len(eps))
	for _, e := range eps {
		list = append(list, MediaEpisodeView{
			FileName: e.FileName, Season: e.Season, Episode: e.Episode, Title: e.Title, Overview: e.Overview,
			StillURL: mediaCoverURL(e.Still),
		})
	}
	out.EpisodeList = &list
	return out, nil
}

func (v *mediaViewer) views(items []model.MediaItem) []MediaItemView {
	out := make([]MediaItemView, len(items))
	for i := range items {
		out[i] = v.view(&items[i])
	}
	return out
}

// childPosters picks the mosaic of a collection. The same rule as the listing
// decides what the user may see, so a poster never shows what the list would
// hide. It looks at a bounded number of covers; a collection whose first ones
// are all unreadable shows fewer than 4.
func (v *mediaViewer) childPosters(coll *model.MediaItem) []string {
	out := []string{}
	covers, err := db.ListVideoCoversUnder(db.MediaItemPath(coll), 10*childPosterCount)
	if err != nil {
		return out
	}
	self := db.MediaItemPath(coll)
	for i := range covers {
		if len(out) == childPosterCount {
			break
		}
		if db.MediaItemPath(&covers[i]) == self || !CanUserAccessItem(v.user, &covers[i]) {
			continue
		}
		out = append(out, mediaCoverURL(covers[i].Cover))
	}
	return out
}

func identityPath(p string) string { return p }

// userRelPath turns a stored absolute path into one relative to the user's
// base path, the way fs paths look to that user.
func userRelPath(user *model.User) func(string) string {
	base := utils.FixAndCleanPath(user.BasePath)
	if base == "/" {
		return identityPath
	}
	return func(p string) string {
		if p == base {
			return "/"
		}
		return strings.TrimPrefix(p, base)
	}
}
