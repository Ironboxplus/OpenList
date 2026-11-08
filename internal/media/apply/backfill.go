package apply

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// BackfillReport is the outcome of BackfillBackdrops.
type BackfillReport struct {
	Scanned int `json:"scanned"` // items at the prefix
	Filled  int `json:"filled"`  // backdrop stored
	Skipped int `json:"skipped"` // locked, no tmdb id, already has one, or TMDB has none
	Failed  int `json:"failed"`
}

// BackfillBackdrops gives the works under prefix that were scraped from TMDB
// before backdrops were kept their backdrop, without identifying them again:
// one TMDB detail request per work, one at a time, only the backdrop is
// written (the rest of the item, its source and its confidence stay). Locked
// works and works that already have a backdrop are not touched, so running it
// again only retries what failed or what TMDB had no backdrop for.
func (o *Orchestrator) BackfillBackdrops(ctx context.Context, prefix string) (*BackfillReport, error) {
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
		if ex.Locked || ex.HasBackdrop || id == "" {
			rep.Skipped++
			continue
		}
		err := o.backfillOne(ctx, p, ex, id, rep)
		if err != nil {
			rep.Failed++
			o.logf("backdrop %s: %v", p, scraper.MaskSecrets(err.Error()))
			if fatal(err) {
				return rep, err
			}
		}
	}
	return rep, nil
}

func (o *Orchestrator) backfillOne(ctx context.Context, p string, ex Existing, id string, rep *BackfillReport) error {
	tg := &scraper.ScrapeTarget{Kind: ex.Kind, ExternalIDs: map[string]string{scraper.ProviderTMDB: id}}
	if err := o.Providers.TMDB.Fill(ctx, tg); err != nil {
		return err
	}
	if tg.BackdropURL == "" {
		rep.Skipped++
		return nil
	}
	backdrop := tg.BackdropURL
	if o.Download != nil {
		data, err := o.Download(ctx, tg.BackdropURL)
		if err != nil {
			return fmt.Errorf("image: %w", err)
		}
		if backdrop, err = o.Writer.SaveImage(ctx, p, "backdrop", data); err != nil {
			return fmt.Errorf("upload: %w", err)
		}
	}
	// not forced: an item locked meanwhile stays as it is
	if err := o.Writer.Set(ctx, model.MediaSetReq{Path: p, Backdrop: &backdrop}, false); err != nil {
		if errors.Is(err, ErrLocked) {
			rep.Skipped++
			return nil
		}
		return err
	}
	rep.Filled++
	return nil
}
