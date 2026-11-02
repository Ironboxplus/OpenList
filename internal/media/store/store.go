// Package store reads and writes the video works of the media library: the
// curation side (rules, scrapers, people), on top of the upstream MediaItem
// model. It is the one place the apply flow and the curate API write through.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// ErrLocked is returned when an automatic write meets a locked item.
var ErrLocked = fmt.Errorf("media item is locked")

// MaxImageBytes caps an uploaded poster or backdrop.
const MaxImageBytes = 10 << 20

// Store writes works and keeps their images the way the library is configured
// to keep covers (media_thumbnail_mode / media_thumbnail_path).
type Store struct {
	ThumbMode string // base64 | runtime | local
	ThumbPath string
}

// New returns a Store keeping images in the given thumbnail mode and path.
func New(thumbMode, thumbPath string) *Store {
	return &Store{ThumbMode: thumbMode, ThumbPath: thumbPath}
}

// SetOptions tunes one Set call.
type SetOptions struct {
	// RespectLock makes Set refuse (ErrLocked) to touch an item whose Locked
	// flag is set. Automatic scraping uses it; an admin's explicit edit does
	// not.
	RespectLock bool
}

// NormalizePath is the one spelling of an item path.
func NormalizePath(p string) string { return utils.FixAndCleanPath(strings.TrimSpace(p)) }

// Get returns the work at path, or nil.
func (s *Store) Get(path string) (*model.MediaItem, error) {
	return db.GetVideoItemByPath(NormalizePath(path))
}

// Set upserts the work at req.Path. Only the fields present in req are
// written; everything else keeps its value. A new work needs a valid Kind.
func (s *Store) Set(req model.MediaSetReq, opt SetOptions) (*model.MediaItem, error) {
	if strings.TrimSpace(req.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	path := NormalizePath(req.Path)
	if path == "/" {
		return nil, fmt.Errorf("the root cannot be a media item")
	}
	it, err := db.GetVideoItemByPath(path)
	if err != nil {
		return nil, err
	}
	if it != nil && it.Locked && opt.RespectLock {
		return it, ErrLocked
	}
	if it == nil {
		if req.Kind == nil {
			return nil, fmt.Errorf("kind is required for a new item")
		}
		if it, err = newItem(path); err != nil {
			return nil, err
		}
	}
	if req.Kind != nil {
		if !model.ValidMediaKind(*req.Kind) {
			return nil, fmt.Errorf("invalid kind %q", *req.Kind)
		}
		switch {
		case *req.Kind == model.MediaKindIgnore:
			it.Hidden = true
		case it.VideoType == model.MediaKindIgnore:
			it.Hidden = false // no longer ignored: show it again
		}
		it.VideoType = *req.Kind
	}
	if req.Title != nil {
		it.ScrapedName = *req.Title
	}
	if req.OriginalTitle != nil {
		it.OriginalTitle = *req.OriginalTitle
	}
	if req.Year != nil {
		it.ReleaseDate = withYear(it.ReleaseDate, *req.Year)
	}
	if req.ExternalIDs != nil {
		it.ExternalIDs = mustJSON(req.ExternalIDs)
		it.ExternalID = upstreamExternalID(req.ExternalIDs)
	}
	var ratings []model.MediaRating
	if req.Ratings != nil {
		ratings = RatingsFromMap(req.Ratings)
		it.Rating = primaryRating(ratings)
	}
	if req.Genres != nil {
		it.Genre = strings.Join(req.Genres, ",")
	}
	if req.Overview != nil {
		it.Plot = *req.Overview
	}
	if req.Cover != nil {
		it.Cover = *req.Cover
	}
	if req.Backdrop != nil {
		it.Backdrop = *req.Backdrop
	}
	if req.Source != nil {
		switch *req.Source {
		case model.MediaSourceManual, model.MediaSourceRule, model.MediaSourceScraper:
		default:
			return nil, fmt.Errorf("invalid source %q", *req.Source)
		}
		it.Source = *req.Source
	}
	if req.Confidence != nil {
		it.Confidence = *req.Confidence
	}
	if req.Locked != nil {
		it.Locked = *req.Locked
	}
	if req.Candidates != nil {
		it.Candidates = mustJSON(req.Candidates)
	}
	if it.Source == model.MediaSourceScraper && (req.Title != nil || req.ExternalIDs != nil) {
		now := time.Now()
		it.ScrapedAt = &now
	}
	var eps []model.MediaEpisode
	if req.Episodes != nil {
		eps = make([]model.MediaEpisode, 0, len(req.Episodes))
		for _, e := range req.Episodes {
			eps = append(eps, model.MediaEpisode{
				FileName: e.FileName, Season: e.Season, Episode: e.Episode,
				Title: e.Title, Overview: e.Overview, Still: e.Still,
			})
		}
		it.Episodes = model.EpisodesJSON(eps)
	}
	if err := db.SaveVideoItem(it, ratings, eps); err != nil {
		return nil, err
	}
	return it, nil
}

func newItem(path string) (*model.MediaItem, error) {
	folder, name := db.SplitMediaPath(path)
	if utf8.RuneCountInString(folder) > model.MediaIndexColumnMax || utf8.RuneCountInString(name) > model.MediaIndexColumnMax {
		return nil, fmt.Errorf("path exceeds %d characters", model.MediaIndexColumnMax)
	}
	it := &model.MediaItem{
		MediaType: model.MediaTypeVideo, FolderPath: folder, FileName: name,
		IsFolder:      !identify.IsVideo(name),
		ScrapedName:   strings.TrimSuffix(name, pathExt(name)),
		MediaCuration: model.MediaCuration{Source: model.MediaSourceManual},
	}
	if sp, err := db.FindScanPathFor(model.MediaTypeVideo, path); err == nil && sp != nil {
		it.ScanPathID = sp.ID
	}
	return it, nil
}

func pathExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[i:]
	}
	return ""
}

// SaveImage stores a poster or backdrop for the work at path as a cover and
// returns the cover reference to put in MediaItem.Cover or Backdrop. role keeps
// the two apart ("poster" or "backdrop"). Where it goes follows the library's
// thumbnail mode.
func (s *Store) SaveImage(ctx context.Context, path, role string, data []byte) (string, error) {
	if int64(len(data)) > MaxImageBytes {
		return "", fmt.Errorf("image is larger than %d bytes", MaxImageBytes)
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("not an image (%s)", mime)
	}
	key := NormalizePath(path)
	if role == "backdrop" {
		key += "#backdrop"
	}
	cover := scraper.SaveThumb(ctx, s.ThumbMode, s.ThumbPath, key, mime, data)
	if cover == "" {
		return "", fmt.Errorf("could not store the image")
	}
	return cover, nil
}

// Delete removes the work at path; it reports how many went.
func (s *Store) Delete(path string) (int64, error) {
	return db.DeleteVideoItemByPath(NormalizePath(path))
}

// DeletePrefix removes the work at prefix and everything below it.
func (s *Store) DeletePrefix(prefix string) (int64, error) {
	return db.DeleteVideoItemsUnder(NormalizePath(prefix))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// withYear puts year into a release date, keeping the rest when the year is
// the same. 0 clears it.
func withYear(date string, year int) string {
	if year <= 0 {
		return ""
	}
	y := fmt.Sprintf("%04d", year)
	if strings.HasPrefix(date, y) {
		return date
	}
	return y + "-01-01"
}

// upstreamExternalID spells the TMDB id the way the upstream scraper does
// ("tmdb:603"), for the pages and APIs that read MediaItem.ExternalID.
func upstreamExternalID(ids map[string]string) string {
	id := ids[scraper.ProviderTMDB]
	if id == "" {
		return ""
	}
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	return "tmdb:" + id
}

// RatingsFromMap turns the ratings of a request ({"tmdb": 8.3, "rt": "93%"})
// into rows, in the order of the sources.
func RatingsFromMap(m map[string]any) []model.MediaRating {
	out := make([]model.MediaRating, 0, len(m))
	for src, v := range m {
		r := model.MediaRating{Source: src}
		switch x := v.(type) {
		case float64:
			r.Value = float32(x)
		case float32:
			r.Value = x
		case int:
			r.Value = float32(x)
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 32); err == nil {
				r.Value = float32(f)
			} else {
				r.Text = x
			}
		default:
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// RatingsToMap is the reverse of RatingsFromMap.
func RatingsToMap(rows []model.MediaRating) map[string]any {
	m := make(map[string]any, len(rows))
	for _, r := range rows {
		if r.Text != "" && r.Value == 0 {
			m[r.Source] = r.Text
		} else {
			m[r.Source] = r.Value
		}
	}
	return m
}

func primaryRating(rows []model.MediaRating) float32 {
	byKey := make(map[string]float32, len(rows))
	for _, r := range rows {
		byKey[r.Source] = r.Value
	}
	return model.PrimaryRating(byKey)
}
