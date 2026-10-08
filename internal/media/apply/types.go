// Package apply is the flow that turns a library directory into a media item:
// plan (look and propose, writes nothing), apply (write a reviewed plan) and
// scrape-one. It talks to storage through a Lister and to the media store
// through a Writer; each has an in-process implementation (inside the server)
// and a remote one (over the HTTP API, for the CLI on a PC).
package apply

import (
	"context"
	"errors"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// ErrLocked is returned when a locked item would be overwritten.
var ErrLocked = errors.New("media item is locked")

// ErrLibrary is returned when a library root itself would be scraped: only its
// entries are works.
var ErrLibrary = errors.New("path is a library root; scrape its entries (media plan --root <path>)")

// ErrNotDir is returned by a Lister for a path that is a file.
var ErrNotDir = errors.New("not a directory")

// Lister lists one directory. It never refreshes the storage cache.
type Lister interface {
	List(ctx context.Context, path string) ([]identify.Entry, error)
}

// Existing is what the store already knows about a path.
type Existing struct {
	Path       string  `json:"path"`
	Kind       string  `json:"kind"`
	Title      string  `json:"title"`
	Locked     bool    `json:"locked"`
	Confidence float32 `json:"confidence"`
}

// Writer reads and writes the media store.
type Writer interface {
	// Existing returns the items at prefix and below, by path.
	Existing(ctx context.Context, prefix string) (map[string]Existing, error)
	// UploadAsset stores an image and returns its asset id.
	UploadAsset(ctx context.Context, data []byte, srcURL string) (uint, error)
	// Set upserts an item. force lets it overwrite a locked one.
	Set(ctx context.Context, req model.MediaSetReq, force bool) error
}

// ServerScraper is implemented by writers that can ask the server to scrape
// (the remote one): used when the caller has no provider keys of its own.
type ServerScraper interface {
	ServerScrape(ctx context.Context, path, provider, id string, lock, force bool) error
}

// Searcher finds candidates for a cleaned name. *scraper.Set is one; the
// remote one asks the server.
type Searcher interface {
	Identify(ctx context.Context, kind string, theatrical bool, q scraper.IdentifyQuery) ([]scraper.Candidate, error)
}

// TMDBResolver turns an inline id tag into a typed TMDB id ("tv/39218").
// *scraper.Set is one; the remote searcher is not (the server has no such
// endpoint), so a tmdb tag is then taken at its word and an imdb tag ignored.
type TMDBResolver interface {
	ResolveTMDB(ctx context.Context, hint, tmdb, imdb string) (string, error)
}

// tmdbHint is the TMDB type an identification most likely is.
func tmdbHint(kind string, theatrical bool) string {
	if kind == "movie" || (kind == "anime" && theatrical) {
		return "movie"
	}
	return "tv"
}

// Downloader fetches an image.
type Downloader func(ctx context.Context, url string) ([]byte, error)

// Orchestrator carries everything a flow needs. Providers may be nil: then
// Apply asks the server to scrape (Writer must be a ServerScraper).
type Orchestrator struct {
	Lister     Lister
	Writer     Writer
	Search     Searcher
	Providers  *scraper.Set
	Classifier identify.Classifier
	Download   Downloader
	Logf       func(format string, args ...any)
}

func (o *Orchestrator) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// search is the Searcher to use: the configured one, else the local providers.
func (o *Orchestrator) search() Searcher {
	if o.Search != nil {
		return o.Search
	}
	if o.Providers != nil {
		return o.Providers
	}
	return nil
}

func (o *Orchestrator) classifier() identify.Classifier {
	if o.Classifier != nil {
		return o.Classifier
	}
	return identify.Rules{}
}
