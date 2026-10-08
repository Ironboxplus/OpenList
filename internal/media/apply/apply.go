package apply

import (
	"context"
	"fmt"
	"path"
	"sort"
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
// provider and id, downloads the poster and backdrop here and uploads them,
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
	case row.Provider == "" || row.Kind == "ignore" || row.Kind == "library":
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
		url string
		dst **uint
	}{{tg.PosterURL, &req.PosterID}, {tg.BackdropURL, &req.BackdropID}} {
		if img.url == "" || o.Download == nil {
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
		id, err := o.Writer.UploadAsset(ctx, data, img.url)
		if err != nil {
			*warns = append(*warns, "upload: "+err.Error())
			continue
		}
		*img.dst = ptr(id)
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

const maxEpisodes = 3000

type fileRef struct {
	rel       string // relative to the item path
	name      string
	dirSeason int
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

	top, err := o.Lister.List(ctx, itemPath)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", itemPath, err)
	}
	var files []fileRef
	var subdirs []identify.Entry
	for _, e := range top {
		switch {
		case e.IsDir:
			if !identify.IsExtraDir(e.Name) {
				subdirs = append(subdirs, e)
			}
		case identify.IsVideo(e.Name):
			files = append(files, fileRef{rel: e.Name, name: e.Name})
		}
	}
	sort.Slice(subdirs, func(i, j int) bool { return subdirs[i].Name < subdirs[j].Name })
	for i, d := range subdirs {
		if i >= 30 {
			break
		}
		if !identify.IsSeasonDir(d.Name) {
			continue
		}
		kids, err := o.Lister.List(ctx, path.Join(itemPath, d.Name))
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", d.Name, err)
		}
		for _, k := range kids {
			if !k.IsDir && identify.IsVideo(k.Name) {
				files = append(files, fileRef{rel: d.Name + "/" + k.Name, name: k.Name, dirSeason: identify.SeasonOfDir(d.Name)})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	var out []model.MediaEpisodeIn
	for _, f := range files {
		s, e, ok := identify.EpisodeOf(f.name)
		if !ok {
			continue
		}
		if s == 0 {
			s = f.dirSeason
		}
		if s == 0 {
			s = 1
		}
		ep := model.MediaEpisodeIn{FileName: f.rel, Season: s, Episode: e}
		if in, found := byKey[[2]int{s, e}]; found {
			ep.Title, ep.Overview = in.Title, in.Overview
		}
		out = append(out, ep)
		if len(out) >= maxEpisodes {
			break
		}
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
