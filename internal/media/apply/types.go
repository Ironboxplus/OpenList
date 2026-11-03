// Package apply is the flow that turns a library directory into a media item:
// plan (look and propose, writes nothing), apply (write a reviewed plan) and
// scrape-one. It talks to storage through a Lister and to the media store
// through a Writer; each has an in-process implementation (inside the server)
// and a remote one (over the HTTP API, for the CLI on a PC).
package apply

import (
	"context"
	"errors"
	"path"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// ErrLocked is returned when a locked item would be overwritten.
var ErrLocked = errors.New("media item is locked")

// ErrGone is returned when a scrape finds its item no longer in the library
// (a rescan removed what it no longer produces while the scrape was running):
// the scrape must not bring the row back.
var ErrGone = errors.New("media item is no longer in the library")

// ErrNotDir is returned by a Lister for a path that is a file.
var ErrNotDir = errors.New("not a directory")

// Lister lists one directory. It never refreshes the storage cache.
type Lister interface {
	List(ctx context.Context, path string) ([]identify.Entry, error)
}

// FileReader is implemented by Listers that can also read the start of a file
// (the NFO next to a work). Remote listers cannot, and then NFOs are not read.
type FileReader interface {
	ReadFile(ctx context.Context, path string, max int64) ([]byte, error)
}

// Existing is what the store already knows about a path.
type Existing struct {
	Path       string  `json:"path"`
	Kind       string  `json:"kind"`
	Title      string  `json:"title"`
	Locked     bool    `json:"locked"`
	Confidence float32 `json:"confidence"`
	// ExternalIDs are the provider ids of a matched work (tmdb "tv/890", imdb "tt...").
	ExternalIDs map[string]string `json:"external_ids,omitempty"`
}

// Writer reads and writes the media store.
type Writer interface {
	// Existing returns the items at prefix and below, by path.
	Existing(ctx context.Context, prefix string) (map[string]Existing, error)
	// SaveImage stores a poster or backdrop of the work at path (role is
	// "poster" or "backdrop") and returns the cover reference to put in the
	// work's Cover or Backdrop.
	SaveImage(ctx context.Context, path, role string, data []byte) (string, error)
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

// classify identifies one path; a classifier that can look into
// subdirectories is given the lister for that.
func (o *Orchestrator) classify(ctx context.Context, p, name string, isDir bool, entries []identify.Entry) (identify.Result, error) {
	cl := o.classifier()
	if pc, ok := cl.(identify.PeekClassifier); ok && isDir && o.Lister != nil {
		return pc.ClassifyWith(ctx, name, isDir, entries, func(child string) ([]identify.Entry, error) {
			return o.Lister.List(ctx, path.Join(p, child))
		})
	}
	return cl.Classify(ctx, name, isDir, entries)
}
