package apply

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// BackfillReport is the outcome of BackfillArtwork.
type BackfillReport struct {
	Scanned   int `json:"scanned"`   // items at the prefix
	Filled    int `json:"filled"`    // items that got something
	Backdrops int `json:"backdrops"` // backdrops stored
	Stills    int `json:"stills"`    // episode stills stored
	Skipped   int `json:"skipped"`   // locked, no tmdb id, nothing missing, or TMDB has none
	Failed    int `json:"failed"`
}

// BackfillArtwork gives the works under prefix that were scraped from TMDB
// before backdrops and stills were kept (a locked work is never touched) their
// missing backdrop and episode stills, without identifying them again: one TMDB
// detail request per work plus one per season for the stills, one work at a
// time. Only empty fields are filled; the rest of the item, its source and its
// confidence stay, and an episode keeps the title and overview it has. Running
// it again only retries what failed or what TMDB had no image for.
func (o *Orchestrator) BackfillArtwork(ctx context.Context, prefix string) (*BackfillReport, error) {
	if o.Providers == nil || o.Providers.TMDB == nil {
		return nil, errors.New("no tmdb provider configured")
	}
	existing, err := o.Writer.Existing(ctx, prefix)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(existing))
	for p := range existing {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	rep := &BackfillReport{Scanned: len(paths)}
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		ex := existing[p]
		id := ex.ExternalIDs[scraper.ProviderTMDB]
		if ex.Locked || id == "" {
			rep.Skipped++
			continue
		}
		if err := o.backfillOne(ctx, p, ex, id, rep); err != nil {
			rep.Failed++
			o.logf("artwork %s: %v", p, scraper.MaskSecrets(err.Error()))
			if fatal(err) {
				return rep, err
			}
		}
	}
	return rep, nil
}

func (o *Orchestrator) backfillOne(ctx context.Context, p string, ex Existing, id string, rep *BackfillReport) error {
	needBackdrop := !ex.HasBackdrop
	var eps []model.MediaEpisodeIn
	needStills := false
	if r, ok := o.Writer.(EpisodeReader); ok && strings.HasPrefix(id, "tv/") {
		var err error
		if eps, err = r.Episodes(ctx, p); err != nil {
			return err
		}
		for _, e := range eps {
			if e.Still == "" {
				needStills = true
				break
			}
		}
	}
	if !needBackdrop && !needStills {
		rep.Skipped++
		return nil
	}

	tg := &scraper.ScrapeTarget{Kind: ex.Kind, ExternalIDs: map[string]string{scraper.ProviderTMDB: id}}
	if err := o.Providers.TMDB.Fill(ctx, tg); err != nil {
		return err
	}
	req := model.MediaSetReq{Path: p}
	var failure error
	if needBackdrop && tg.BackdropURL != "" {
		backdrop, err := o.fetchImage(ctx, p, "backdrop", tg.BackdropURL)
		if err != nil {
			if fatal(err) {
				return err
			}
			failure = err
		} else {
			req.Backdrop = &backdrop
		}
	}
	stills := 0
	if needStills {
		info, ierr := o.Providers.Episodes(ctx, tg) // what was read of the seasons is used even when one failed
		if ierr != nil && fatal(ierr) {
			return ierr
		}
		if ierr != nil {
			failure = ierr
		}
		byKey := make(map[[2]int]scraper.EpisodeInfo, len(info))
		for _, e := range info {
			byKey[[2]int{e.Season, e.Episode}] = e
		}
		got := map[[2]int]string{}
		failed, changed := 0, false
		for i := range eps {
			e := &eps[i]
			if e.Still != "" {
				continue
			}
			k := [2]int{e.Season, e.Episode}
			in, ok := byKey[k]
			if !ok {
				continue
			}
			if e.Title == "" && in.Title != "" {
				e.Title, changed = in.Title, true
			}
			if e.Overview == "" && in.Overview != "" {
				e.Overview, changed = in.Overview, true
			}
			if in.StillURL == "" || failed >= maxStillFailures {
				continue
			}
			cover, done := got[k]
			if !done {
				var err error
				if cover, err = o.fetchImage(ctx, p, StillRole(e.Season, e.Episode), in.StillURL); err != nil {
					if fatal(err) {
						return err
					}
					failed++
					failure = err
					continue
				}
				got[k] = cover
				stills++
			}
			e.Still = cover
		}
		if stills > 0 || changed {
			req.Episodes = eps
		}
	}
	if req.Backdrop == nil && req.Episodes == nil {
		if failure != nil {
			return failure
		}
		rep.Skipped++
		return nil
	}
	// not forced: an item locked meanwhile stays as it is
	if err := o.Writer.Set(ctx, req, false); err != nil {
		if errors.Is(err, ErrLocked) {
			rep.Skipped++
			return nil
		}
		return err
	}
	rep.Filled++
	rep.Stills += stills
	if req.Backdrop != nil {
		rep.Backdrops++
	}
	return failure
}
