// Package store reads and writes media items and their image assets. It is the
// only place that touches the media tables; handlers and the apply flow go
// through it.
package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// ErrLocked is returned when an automatic write meets a locked item.
var ErrLocked = fmt.Errorf("media item is locked")

// Store holds the directory the asset files live in.
type Store struct {
	AssetDir      string
	MaxAssetBytes int64 // 0 = 10 MiB
}

// New returns a Store keeping assets under assetDir.
func New(assetDir string) *Store { return &Store{AssetDir: assetDir} }

// SetOptions tunes one Set call.
type SetOptions struct {
	// RespectLock makes Set refuse (ErrLocked) to touch an item whose Locked
	// flag is set. Automatic scraping uses it; an admin's explicit edit does
	// not.
	RespectLock bool
}

// NormalizePath is the one spelling of an item path.
func NormalizePath(p string) string { return utils.FixAndCleanPath(strings.TrimSpace(p)) }

// Get returns the item at path, or nil.
func (s *Store) Get(path string) (*model.MediaItem, error) {
	return db.GetMediaItemByPath(NormalizePath(path))
}

// Set upserts the item at req.Path. Only the fields present in req are
// written; everything else keeps its value. A new item needs a valid Kind.
func (s *Store) Set(req model.MediaSetReq, opt SetOptions) (*model.MediaItem, error) {
	if strings.TrimSpace(req.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	path := NormalizePath(req.Path)
	if path == "/" {
		return nil, fmt.Errorf("the root cannot be a media item")
	}
	it, err := db.GetMediaItemByPath(path)
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
		it = &model.MediaItem{Path: path, Source: model.MediaSourceManual}
	}
	if req.Kind != nil {
		if !model.ValidMediaKind(*req.Kind) {
			return nil, fmt.Errorf("invalid kind %q", *req.Kind)
		}
		it.Kind = *req.Kind
	}
	if req.Title != nil {
		it.Title = *req.Title
	}
	if req.OriginalTitle != nil {
		it.OriginalTitle = *req.OriginalTitle
	}
	if req.Year != nil {
		it.Year = *req.Year
	}
	if req.ExternalIDs != nil {
		it.ExternalIDs = mustJSON(req.ExternalIDs)
	}
	if req.Ratings != nil {
		it.Ratings = mustJSON(req.Ratings)
	}
	if req.Genres != nil {
		it.Genres = mustJSON(req.Genres)
	}
	if req.Overview != nil {
		it.Overview = *req.Overview
	}
	if req.PosterID != nil {
		it.PosterID = *req.PosterID
	}
	if req.BackdropID != nil {
		it.BackdropID = *req.BackdropID
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
				Title: e.Title, Overview: e.Overview, StillID: e.StillID,
			})
		}
	}
	if err := db.SaveMediaItem(it, eps); err != nil {
		return nil, err
	}
	return it, nil
}

// Delete removes the item at path; it reports how many items went.
func (s *Store) Delete(path string) (int64, error) {
	return db.DeleteMediaItemByPath(NormalizePath(path))
}

// DeletePrefix removes the item at prefix and everything below it.
func (s *Store) DeletePrefix(prefix string) (int64, error) {
	return db.DeleteMediaItemsByPrefix(NormalizePath(prefix))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
