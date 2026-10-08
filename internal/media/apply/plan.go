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

// PlanOpts tunes Plan.
type PlanOpts struct {
	Root  string
	Depth int // 1 = the entries of Root; 2 also plans the members of collections
}

// PlanResult is what Plan found. Nothing was written.
type PlanResult struct {
	Rows    []Row
	Skipped []string // paths left alone because their item is locked
}

// Plan looks at the first level under Root (plus, per directory, the one
// listing needed to tell its kind) and proposes an item for each entry. It is
// serial, never refreshes storage, and does not list locked items at all.
// On a network or auth failure it returns the rows so far with the error.
func (o *Orchestrator) Plan(ctx context.Context, opts PlanOpts) (*PlanResult, error) {
	root := path.Clean("/" + strings.TrimLeft(strings.ReplaceAll(opts.Root, "\\", "/"), "/"))
	depth := max(opts.Depth, 1)
	existing, err := o.Writer.Existing(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("read existing media items: %w", err)
	}
	top, err := o.Lister.List(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", root, err)
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Name < top[j].Name })
	res := &PlanResult{}
	for _, e := range top {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := o.planEntry(ctx, root, e, depth, existing, res); err != nil {
			return res, err
		}
	}
	return res, nil
}

func fatal(err error) bool {
	switch scraper.ClassOf(err) {
	case scraper.ClassNetwork, scraper.ClassAuth, scraper.ClassCanceled:
		return true
	}
	return false
}

func (o *Orchestrator) planEntry(ctx context.Context, parent string, e identify.Entry, depthLeft int, existing map[string]Existing, res *PlanResult) error {
	p := path.Join(parent, e.Name)
	if ex, ok := existing[p]; ok && ex.Locked {
		res.Skipped = append(res.Skipped, p)
		return nil
	}
	if !e.IsDir {
		if !identify.IsVideo(e.Name) {
			return nil
		}
		return o.emit(ctx, p, identify.Classify(e.Name, false, nil), "", res)
	}
	lst, lerr := o.Lister.List(ctx, p)
	note := ""
	if lerr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		o.logf("list %s: %v", p, lerr)
		note = "could not list: " + lerr.Error()
		lst = nil
	}
	r, err := o.classifier().Classify(ctx, e.Name, true, lst)
	if err != nil {
		return err
	}
	if err := o.emit(ctx, p, r, note, res); err != nil {
		return err
	}
	if r.Kind == "collection" && depthLeft > 1 {
		kids := append([]identify.Entry(nil), lst...)
		sort.Slice(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
		for _, k := range kids {
			if k.IsDir && identify.IsExtraDir(k.Name) {
				continue
			}
			if err := o.planEntry(ctx, p, k, depthLeft-1, existing, res); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *Orchestrator) emit(ctx context.Context, p string, r identify.Result, note string, res *PlanResult) error {
	row, err := o.resolve(ctx, p, r)
	if note != "" {
		row.Note = strings.TrimSpace(note + "; " + row.Note)
		row.Confidence = min(row.Confidence, 0.59)
	}
	res.Rows = append(res.Rows, row)
	return err
}

// resolve turns an identification into a plan row, searching the providers
// for movies, shows and anime. A fatal provider error is returned after the
// row (without provider data) is filled in.
func (o *Orchestrator) resolve(ctx context.Context, p string, r identify.Result) (Row, error) {
	row := Row{Path: p, Kind: r.Kind, Title: r.Title, Year: r.Year, Confidence: r.Confidence, Note: "rule: " + r.Why}
	if r.Kind == "ignore" || r.Kind == "collection" || o.search() == nil {
		return row, nil
	}
	q := scraper.IdentifyQuery{Terms: r.Terms(), Year: r.Year, English: r.English}
	cs, err := o.search().Identify(ctx, r.Kind, r.Theatrical, q)
	if err != nil && fatal(err) {
		row.Confidence = 0
		row.Note = "search failed: " + err.Error()
		return row, err
	}
	// A show TMDB can't place may be an anime Bangumi knows.
	if r.Kind == "tv" && (len(cs) == 0 || cs[0].Score < scraper.StrongMatch) {
		if bs, berr := o.search().Identify(ctx, "anime", false, q); berr == nil && len(bs) > 0 && bs[0].Provider == scraper.ProviderBGM && bs[0].Score >= scraper.StrongMatch {
			cs, err = bs, nil
			row.Kind = "anime"
		} else if berr != nil && fatal(berr) {
			return row, berr
		}
	}
	if len(cs) == 0 {
		row.Confidence = 0
		row.Note = "no match"
		if err != nil {
			row.Note = "search failed: " + err.Error()
		}
		return row, nil
	}
	best := cs[0]
	row.Provider, row.ID, row.Title, row.Confidence = best.Provider, best.ID, best.Title, best.Score
	if best.Year > 0 {
		row.Year = best.Year
	}
	if best.Score < model.MediaPendingConfidence {
		row.Note = candidateNote(cs, 3)
	} else {
		row.Note = "rule: " + r.Why
	}
	if r.Confidence < model.MediaPendingConfidence {
		row.Confidence = min(row.Confidence, 0.59)
		row.Note += " [kind guessed from the name]"
	}
	return row, nil
}

func candidateNote(cs []scraper.Candidate, n int) string {
	var parts []string
	for i, c := range cs {
		if i >= n {
			break
		}
		y := ""
		if c.Year > 0 {
			y = fmt.Sprintf(" %d", c.Year)
		}
		parts = append(parts, fmt.Sprintf("%s:%s %s%s", c.Provider, c.ID, c.Title, y))
	}
	return strings.Join(parts, " | ")
}
