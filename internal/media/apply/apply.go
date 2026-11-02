package apply

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// ApplyOpts tunes Apply.
type ApplyOpts struct {
	DryRun bool
	NoLock bool // by default the rows are human-reviewed and written locked
}

// Report is the outcome of Apply.
type Report struct {
	Applied int
	Failed  int
	Lines   []string
}

// Apply writes a reviewed plan. For every row it fetches the details by
// provider and id, downloads the poster and backdrop here and hands them to the Writer,
// and stores the item (locked, source manual) with its episode map. Running it
// again with the same plan gives the same result: assets deduplicate by hash
// and Set overwrites in place. With DryRun nothing is fetched or written.
func (o *Orchestrator) Apply(ctx context.Context, rows []Row, opts ApplyOpts) (*Report, error) {
	rep := &Report{}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		line, err := o.applyRow(ctx, row, opts)
		if err != nil {
			rep.Failed++
			rep.Lines = append(rep.Lines, fmt.Sprintf("FAIL %s: %v", row.Path, err))
			if fatal(err) {
				return rep, err
			}
			continue
		}
		rep.Applied++
		rep.Lines = append(rep.Lines, line)
	}
	return rep, nil
}

func (o *Orchestrator) applyRow(ctx context.Context, row Row, opts ApplyOpts) (string, error) {
	lock := !opts.NoLock
	kind := row.Kind
	req := model.MediaSetReq{
		Path: row.Path, Kind: &kind, Locked: &lock,
		Source: ptr(model.MediaSourceManual), Confidence: ptr(float32(1)),
		Candidates: []model.MediaCandidate{},
	}
	if row.Title != "" {
		req.Title = ptr(row.Title)
	}
	if row.Year > 0 {
		req.Year = ptr(row.Year)
	}
	what := fmt.Sprintf("%s [%s]", row.Path, row.Kind)

	switch {
	case row.Provider == "" || row.Kind == "ignore":
		if opts.DryRun {
			return "would set " + what, nil
		}
		if err := o.Writer.Set(ctx, req, true); err != nil {
			return "", err
		}
		return "set " + what, nil

	case o.Providers == nil:
		ss, ok := o.Writer.(ServerScraper)
		if !ok {
			return "", fmt.Errorf("row has %s:%s but no provider keys are available (pass --env)", row.Provider, row.ID)
		}
		if opts.DryRun {
			return fmt.Sprintf("would ask the server to scrape %s from %s:%s", what, row.Provider, row.ID), nil
		}
		if err := ss.ServerScrape(ctx, row.Path, row.Provider, row.ID, lock, true); err != nil {
			return "", err
		}
		if err := o.Writer.Set(ctx, req, true); err != nil {
			return "", err
		}
		return fmt.Sprintf("scraped on the server and set %s from %s:%s", what, row.Provider, row.ID), nil
	}

	if opts.DryRun {
		return fmt.Sprintf("would fetch %s:%s and set %s", row.Provider, row.ID, what), nil
	}
	tg, warns, err := o.Providers.Fetch(ctx, row.Provider, row.ID, row.Kind)
	if err != nil {
		return "", err
	}
	if err := o.fillRequest(ctx, &req, row, tg, &warns); err != nil {
		return "", err
	}
	if err := o.Writer.Set(ctx, req, true); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("set %s from %s:%s", what, row.Provider, row.ID)
	if len(warns) > 0 {
		msg += " (" + strings.Join(warns, "; ") + ")"
	}
	return msg, nil
}

// fillRequest copies a fetched target into a set request: details, uploaded
// images and, for shows, the episode map. Image and episode trouble is added
// to warns; it does not fail the item.
func (o *Orchestrator) fillRequest(ctx context.Context, req *model.MediaSetReq, row Row, tg *scraper.ScrapeTarget, warns *[]string) error {
	if req.Title == nil || *req.Title == "" {
		req.Title = ptr(tg.Title)
	}
	req.OriginalTitle = ptr(tg.OriginalTitle)
	if req.Year == nil && tg.Year > 0 {
		req.Year = ptr(tg.Year)
	}
	req.ExternalIDs = nonNilStrings(tg.ExternalIDs)
	req.Ratings = tg.Ratings
	if req.Ratings == nil {
		req.Ratings = map[string]any{}
	}
	req.Genres = tg.Genres
	if req.Genres == nil {
		req.Genres = []string{}
	}
	req.Overview = ptr(tg.Overview)

	for _, img := range []struct {
		url, role string
		dst       **string
	}{{tg.PosterURL, "poster", &req.Cover}, {tg.BackdropURL, "backdrop", &req.Backdrop}} {
		if img.url == "" {
			continue
		}
		if o.Download == nil {
			// no way to fetch here: the work keeps the provider's URL
			*img.dst = ptr(img.url)
			continue
		}
		data, err := o.Download(ctx, img.url)
		if err != nil {
			if fatal(err) {
				return err
			}
			*warns = append(*warns, "image: "+scraper.MaskSecrets(err.Error()))
			continue
		}
		cover, err := o.Writer.SaveImage(ctx, row.Path, img.role, data)
		if err != nil {
			*warns = append(*warns, "upload: "+err.Error())
			continue
		}
		*img.dst = ptr(cover)
	}

	if (row.Kind == "tv" || row.Kind == "anime") && o.Lister != nil && tg.Kind != "movie" && !strings.HasPrefix(tg.ExternalIDs[scraper.ProviderTMDB], "movie/") {
		eps, err := o.mapEpisodes(ctx, row.Path, tg)
		if err != nil {
			if fatal(err) {
				return err
			}
			*warns = append(*warns, "episodes: "+err.Error())
		}
		if len(eps) > 0 {
			req.Episodes = eps
		}
	}
	return nil
}

// mapEpisodes lists the item's directory (and its season directories, one
// level) and pairs every episode-looking video with the provider's episode
// info by season and number.
func (o *Orchestrator) mapEpisodes(ctx context.Context, itemPath string, tg *scraper.ScrapeTarget) ([]model.MediaEpisodeIn, error) {
	info, ierr := o.Providers.Episodes(ctx, tg)
	byKey := map[[2]int]scraper.EpisodeInfo{}
	for _, e := range info {
		byKey[[2]int{e.Season, e.Episode}] = e
	}

	files, err := identify.EpisodeFiles(func(rel string) ([]identify.Entry, error) {
		return o.Lister.List(ctx, path.Join(itemPath, rel))
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", itemPath, err)
	}
	out := make([]model.MediaEpisodeIn, 0, len(files))
	for _, f := range files {
		ep := model.MediaEpisodeIn{FileName: f.Rel, Season: f.Season, Episode: f.Episode}
		if in, found := byKey[[2]int{f.Season, f.Episode}]; found {
			ep.Title, ep.Overview = in.Title, in.Overview
		}
		out = append(out, ep)
	}
	return out, ierr
}

func ptr[T any](v T) *T { return &v }

func nonNilStrings(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
