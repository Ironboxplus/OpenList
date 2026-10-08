package apply

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// ScrapeOpts tunes ScrapeOne.
type ScrapeOpts struct {
	Provider string // with ID: fetch this exact item
	ID       string
	Lock     bool // lock the item afterwards
	Force    bool // overwrite a locked item
}

// ScrapeResult is what ScrapeOne did.
type ScrapeResult struct {
	Path       string              `json:"path"`
	Matched    bool                `json:"matched"` // false: stored with candidates for a human to pick
	Provider   string              `json:"provider,omitempty"`
	ID         string              `json:"id,omitempty"`
	Title      string              `json:"title,omitempty"`
	Kind       string              `json:"kind"`
	Confidence float32             `json:"confidence"`
	Candidates []scraper.Candidate `json:"candidates,omitempty"`
	Warnings   []string            `json:"warnings,omitempty"`
}

// ScrapeOne identifies and scrapes one path and stores the result.
//
// With a provider and id it fetches that item (a person's explicit choice:
// source manual, confidence 1). Without, it identifies the name and searches:
// a strong match is fetched and stored (source scraper); a weak one is stored
// with its top candidates and low confidence, to be picked later. A locked
// item is never touched unless Force.
func (o *Orchestrator) ScrapeOne(ctx context.Context, p string, opts ScrapeOpts) (*ScrapeResult, error) {
	if o.Providers == nil {
		return nil, errors.New("no scraper providers configured")
	}
	p = path.Clean("/" + strings.TrimLeft(strings.ReplaceAll(p, "\\", "/"), "/"))
	ex, err := o.Writer.Existing(ctx, p)
	if err != nil {
		return nil, err
	}
	if cur, ok := ex[p]; ok && cur.Locked && !opts.Force {
		return nil, ErrLocked
	}

	lock := opts.Lock
	res := &ScrapeResult{Path: p}
	name := path.Base(p)
	explicit := opts.Provider != "" && opts.ID != ""

	// What is it? List it once (a loose file has no listing).
	isDir := true
	entries, lerr := o.Lister.List(ctx, p)
	if lerr != nil {
		switch {
		case errors.Is(lerr, ErrNotDir) || identify.IsVideo(name):
			isDir, entries = false, nil
		case explicit:
			// the user chose the match; a failed listing only costs the kind guess
			o.logf("list %s: %v (continuing with the name only)", p, lerr)
			entries = nil
		default:
			return nil, fmt.Errorf("list %s: %w", p, lerr)
		}
	}
	r, err := o.classifier().Classify(ctx, name, isDir, entries)
	if err != nil {
		return nil, err
	}
	res.Kind = r.Kind

	req := model.MediaSetReq{Path: p, Kind: &r.Kind, Locked: &lock}

	if !explicit {
		if r.Kind == "ignore" || r.Kind == "collection" {
			src := model.MediaSourceRule
			req.Title, req.Year, req.Source, req.Confidence = ptr(r.Title), ptr(r.Year), &src, ptr(r.Confidence)
			if err := o.Writer.Set(ctx, req, opts.Force); err != nil {
				return nil, err
			}
			res.Title, res.Confidence, res.Matched = r.Title, r.Confidence, true
			return res, nil
		}
		cs, err := o.search().Identify(ctx, r.Kind, r.Theatrical, scraper.IdentifyQuery{Terms: r.Terms(), Year: r.Year, English: r.English})
		if err != nil && len(cs) == 0 {
			return nil, err
		}
		if len(cs) == 0 {
			src := model.MediaSourceRule
			req.Title, req.Year, req.Source, req.Confidence = ptr(r.Title), ptr(r.Year), &src, ptr(float32(0))
			if err := o.Writer.Set(ctx, req, opts.Force); err != nil {
				return nil, err
			}
			res.Title = r.Title
			return res, nil
		}
		best := cs[0]
		if best.Score < scraper.StrongMatch {
			// Keep the guess and the candidates; leave the fields to a human.
			src := model.MediaSourceRule
			top := cs
			if len(top) > 5 {
				top = top[:5]
			}
			req.Title, req.Year, req.Source = ptr(r.Title), ptr(r.Year), &src
			req.Confidence = ptr(best.Score)
			req.Candidates = toModelCandidates(top)
			if err := o.Writer.Set(ctx, req, opts.Force); err != nil {
				return nil, err
			}
			res.Title, res.Confidence, res.Candidates = r.Title, best.Score, top
			return res, nil
		}
		opts.Provider, opts.ID = best.Provider, best.ID
		res.Confidence = best.Score
		res.Candidates = nil
	} else {
		res.Confidence = 1
	}

	tg, warns, err := o.Providers.Fetch(ctx, opts.Provider, opts.ID, r.Kind)
	if err != nil {
		return nil, err
	}
	// The chosen match knows better than the name whether it is a show.
	if id := tg.ExternalIDs[scraper.ProviderTMDB]; r.Kind == "movie" && strings.HasPrefix(id, "tv/") {
		r.Kind = "tv"
	} else if r.Kind == "tv" && strings.HasPrefix(id, "movie/") {
		r.Kind = "movie"
	}
	res.Kind = r.Kind
	row := Row{Path: p, Kind: r.Kind, Provider: opts.Provider, ID: opts.ID}
	src := model.MediaSourceScraper
	conf := res.Confidence
	if explicit {
		src = model.MediaSourceManual
	}
	req.Source, req.Confidence, req.Candidates = &src, &conf, []model.MediaCandidate{}
	if err := o.fillRequest(ctx, &req, row, tg, &warns); err != nil {
		return nil, err
	}
	if err := o.Writer.Set(ctx, req, opts.Force); err != nil {
		return nil, err
	}
	res.Matched, res.Provider, res.ID, res.Title, res.Warnings = true, opts.Provider, opts.ID, tg.Title, warns
	return res, nil
}

func toModelCandidates(cs []scraper.Candidate) []model.MediaCandidate {
	out := make([]model.MediaCandidate, 0, len(cs))
	for _, c := range cs {
		out = append(out, model.MediaCandidate{Provider: c.Provider, ID: c.ID, Title: c.Title, Year: c.Year, PosterURL: c.PosterURL, Score: c.Score})
	}
	return out
}
