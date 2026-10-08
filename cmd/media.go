package cmd

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/media/apply"
	"github.com/OpenListTeam/OpenList/v4/internal/media/check"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/spf13/cobra"
)

// The media commands run on any machine that can reach the server: they read
// directories and write results through the HTTP API (the same client as the
// sync commands). Provider keys come from --env on this machine, so a server
// that can't reach TMDB or Bangumi is no obstacle.

var mediaFlags struct {
	env      string
	root     string
	depth    int
	dryRun   bool
	noLock   bool
	path     string
	provider string
	id       string
	lock     bool
	force    bool
	prefix   string
	pending  bool
	rules    string
	probe    bool
	out      string
}

// Test hooks: point the providers at local servers and allow loopback image
// downloads.
var (
	mediaConfigHook   func(*scraper.Config)
	mediaAllowPrivate bool
)

var mediaCmd = &cobra.Command{
	Use:   "media",
	Short: "Plan, apply and scrape media info for library directories",
	Long: `Identify what the directories under a root are (movie, show, anime,
collection), look them up on TMDB / Bangumi / OMDb / Douban, and store the
result in OpenList's media tables.

  openlist media plan  --root /115/downloads --env media.env > plan.tsv
  (edit plan.tsv: fix kind / provider / id, set kind=ignore to skip a row)
  openlist media apply plan.tsv --env media.env [--dry-run]

plan writes nothing. apply writes the reviewed rows locked, source "manual",
and can be repeated. --env is a KEY=VALUE file with TMDB_READ_TOKEN,
OMDB_KEYS and DOUBAN_APIKEY; its contents are never printed. Without --env the
server's own keys are used (the server must be able to reach the providers).`,
}

func init() {
	RootCmd.AddCommand(mediaCmd)

	plan := &cobra.Command{
		Use:   "plan",
		Short: "Propose a media item for each entry under --root (writes nothing; prints TSV)",
		Args:  cobra.NoArgs,
		RunE:  runMediaPlan,
	}
	plan.Flags().StringVar(&mediaFlags.root, "root", "", "directory to plan, e.g. /115/downloads (required)")
	plan.Flags().IntVar(&mediaFlags.depth, "depth", 1, "1: the entries of --root; 2: also the members of collections")
	_ = plan.MarkFlagRequired("root")

	app := &cobra.Command{
		Use:   "apply <plan.tsv>",
		Short: "Write a reviewed plan: fetch details and images, then store the items locked",
		Args:  cobra.ExactArgs(1),
		RunE:  runMediaApply,
	}
	app.Flags().BoolVarP(&mediaFlags.dryRun, "dry-run", "n", false, "show what would be written without fetching or writing anything")
	app.Flags().BoolVar(&mediaFlags.noLock, "no-lock", false, "do not lock the written items")

	scrape := &cobra.Command{
		Use:   "scrape",
		Short: "Scrape one path (identify it, or fetch --provider/--id)",
		Args:  cobra.NoArgs,
		RunE:  runMediaScrape,
	}
	scrape.Flags().StringVar(&mediaFlags.path, "path", "", "the directory or file to scrape (required)")
	scrape.Flags().StringVar(&mediaFlags.provider, "provider", "", "tmdb or bgm; with --id, fetch exactly this item")
	scrape.Flags().StringVar(&mediaFlags.id, "id", "", `provider id, e.g. "tv/890" for tmdb or "265" for bgm`)
	scrape.Flags().BoolVar(&mediaFlags.lock, "lock", false, "lock the item afterwards")
	scrape.Flags().BoolVar(&mediaFlags.force, "force", false, "overwrite an item that is locked")
	_ = scrape.MarkFlagRequired("path")

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List stored media items",
		Args:  cobra.NoArgs,
		RunE:  runMediaLs,
	}
	ls.Flags().StringVar(&mediaFlags.prefix, "prefix", "", "only items at or below this path")
	ls.Flags().BoolVar(&mediaFlags.pending, "pending", false, "only items that still need a look (unlocked, low confidence or with candidates)")

	chk := &cobra.Command{
		Use:   "check",
		Short: "Report missing episodes, unmatched files and subtitles of stored works (writes nothing)",
		Long: `For every stored work under --root (kind ignore and collection are skipped),
list its directory (up to 3 levels, one request at a time), match the video
files to episode numbers, compare them with the episodes TMDB or Bangumi
list, and look at subtitles. The report is markdown.

  openlist media check --root /115/downloads --env media.env --out report.md
  openlist media check --root /115/downloads --env media.env --rules rules.yaml --probe-embedded --out report.md

--env gives the provider keys; without it the expectation comes from rules
or from the episode numbers found. --rules overrides the defaults per work
(files: rclone filter rules, episode: regexp with (?P<ep>..), expect:
"S1: 1-12", subs: {langs, mode}). --probe-embedded reads the first 4 MB of the
first mkv of a work whose subtitles are not settled by outside files: at most
one /api/fs/get and one Range request per work.`,
		Args: cobra.NoArgs,
		RunE: runMediaCheck,
	}
	chk.Flags().StringVar(&mediaFlags.root, "root", "", "directory holding the works, e.g. /115/downloads (required)")
	chk.Flags().StringVar(&mediaFlags.rules, "rules", "", "rules.yaml with per-work overrides")
	chk.Flags().BoolVar(&mediaFlags.probe, "probe-embedded", false, "read the head of the first mkv of each work to find built-in subtitle tracks")
	chk.Flags().StringVar(&mediaFlags.out, "out", "", "write the markdown report here (default: stdout)")
	_ = chk.MarkFlagRequired("root")

	for _, c := range []*cobra.Command{plan, app, scrape, ls, chk} {
		addServerFlags(c.Flags())
		mediaCmd.AddCommand(c)
	}
	for _, c := range []*cobra.Command{plan, app, scrape, chk} {
		c.Flags().StringVar(&mediaFlags.env, "env", "", "KEY=VALUE file with TMDB_READ_TOKEN, OMDB_KEYS, DOUBAN_APIKEY (default: use the server's keys)")
	}
}

// mediaOrchestrator wires the flow: listing and writing go over HTTP; with
// --env the providers run here, without it searches go through the server.
func mediaOrchestrator(c *syncClient) (*apply.Orchestrator, error) {
	scraper.Version = conf.Version
	remote := apply.Remote{API: c}
	o := &apply.Orchestrator{
		Lister: remote, Writer: remote, Search: remote,
		Logf: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	}
	if mediaFlags.env == "" {
		return o, nil
	}
	env, err := scraper.LoadEnvFile(mediaFlags.env)
	if err != nil {
		return nil, err
	}
	cfg := scraper.ConfigFromEnv(env)
	if mediaConfigHook != nil {
		mediaConfigHook(&cfg)
	}
	set, err := scraper.NewSet(cfg)
	if err != nil {
		return nil, err
	}
	if set.TMDB == nil {
		fmt.Fprintf(os.Stderr, "note: %s has no %s; movies and shows can't be searched, only Bangumi works\n", mediaFlags.env, scraper.EnvTMDBToken)
	}
	dl, err := apply.NewDownloader(apply.DownloadOpts{Proxy: cfg.Proxy, Timeout: cfg.Timeout, AllowPrivate: mediaAllowPrivate})
	if err != nil {
		return nil, err
	}
	o.Providers, o.Search, o.Download = set, set, dl
	return o, nil
}

func mediaContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func runMediaPlan(cmd *cobra.Command, args []string) error {
	c, err := newSyncClient()
	if err != nil {
		return err
	}
	o, err := mediaOrchestrator(c)
	if err != nil {
		return err
	}
	ctx, stop := mediaContext()
	defer stop()
	res, err := o.Plan(ctx, apply.PlanOpts{Root: mediaFlags.root, Depth: mediaFlags.depth})
	if res != nil {
		// rows found before a failure are still worth keeping
		if werr := apply.FormatTSV(cmd.OutOrStdout(), res.Rows); werr != nil && err == nil {
			err = werr
		}
		pending := 0
		for _, r := range res.Rows {
			if r.Confidence < 0.6 {
				pending++
			}
		}
		fmt.Fprintf(os.Stderr, "%d rows (%d below 0.6 confidence, check those), %d locked items skipped\n", len(res.Rows), pending, len(res.Skipped))
	}
	return err
}

func runMediaApply(cmd *cobra.Command, args []string) error {
	f, err := os.Open(args[0])
	if err != nil {
		return err
	}
	rows, err := apply.ParseTSV(f)
	f.Close()
	if err != nil {
		return err
	}
	c, err := newSyncClient()
	if err != nil {
		return err
	}
	o, err := mediaOrchestrator(c)
	if err != nil {
		return err
	}
	ctx, stop := mediaContext()
	defer stop()
	rep, err := o.Apply(ctx, rows, apply.ApplyOpts{DryRun: mediaFlags.dryRun, NoLock: mediaFlags.noLock})
	if rep != nil {
		for _, l := range rep.Lines {
			fmt.Fprintln(cmd.OutOrStdout(), l)
		}
		verb := "applied"
		if mediaFlags.dryRun {
			verb = "would apply"
		}
		fmt.Fprintf(os.Stderr, "%s %d rows, %d failed\n", verb, rep.Applied, rep.Failed)
		if err == nil && rep.Failed > 0 {
			err = fmt.Errorf("%d rows failed", rep.Failed)
		}
	}
	return err
}

func runMediaScrape(cmd *cobra.Command, args []string) error {
	if (mediaFlags.provider == "") != (mediaFlags.id == "") {
		return fmt.Errorf("--provider and --id go together")
	}
	c, err := newSyncClient()
	if err != nil {
		return err
	}
	o, err := mediaOrchestrator(c)
	if err != nil {
		return err
	}
	ctx, stop := mediaContext()
	defer stop()
	if o.Providers == nil {
		// no keys here: let the server scrape
		var res apply.ScrapeResult
		err := c.Call("POST", "/api/admin/media/scrape", map[string]any{
			"path": mediaFlags.path, "provider": mediaFlags.provider, "id": mediaFlags.id,
			"lock": mediaFlags.lock, "force": mediaFlags.force,
		}, &res)
		if err != nil {
			return err
		}
		return printScrape(cmd.OutOrStdout(), &res)
	}
	res, err := o.ScrapeOne(ctx, mediaFlags.path, apply.ScrapeOpts{Provider: mediaFlags.provider, ID: mediaFlags.id, Lock: mediaFlags.lock, Force: mediaFlags.force})
	if err != nil {
		if err == apply.ErrLocked {
			return fmt.Errorf("%s is locked; use --force to overwrite it", mediaFlags.path)
		}
		return err
	}
	return printScrape(cmd.OutOrStdout(), res)
}

func runMediaCheck(cmd *cobra.Command, args []string) error {
	var rules check.Rules
	if mediaFlags.rules != "" {
		var err error
		if rules, err = check.LoadRules(mediaFlags.rules); err != nil {
			return err
		}
	}
	c, err := newSyncClient()
	if err != nil {
		return err
	}
	o, err := mediaOrchestrator(c)
	if err != nil {
		return err
	}
	if o.Providers == nil {
		fmt.Fprintln(os.Stderr, "note: no --env, so expected episodes come from rules or from the numbers found only")
	}
	ck := &check.Checker{
		Items: check.RemoteItems{API: c}, Lister: o.Lister, Providers: o.Providers, Rules: rules,
		Logf: o.Logf,
	}
	if mediaFlags.probe {
		ck.Prober = check.RemoteProber{API: c, Base: c.base, Token: c.token, HTTP: c.http}
	}
	ctx, stop := mediaContext()
	defer stop()
	results, err := ck.Run(ctx, mediaFlags.root)
	if len(results) == 0 && err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if mediaFlags.out != "" {
		f, ferr := os.Create(mediaFlags.out)
		if ferr != nil {
			return ferr
		}
		defer f.Close()
		w = f
	}
	// a run cut short still reports what it saw
	if werr := check.WriteReport(w, mediaFlags.root, results, time.Now()); werr != nil && err == nil {
		err = werr
	}
	return err
}

func printScrape(w io.Writer, r *apply.ScrapeResult) error {
	if r.Matched {
		fmt.Fprintf(w, "%s: %s [%s] from %s:%s\n", r.Path, r.Title, r.Kind, r.Provider, r.ID)
	} else {
		fmt.Fprintf(w, "%s: no confident match (%.2f); stored as %s with %d candidates to pick from\n", r.Path, r.Confidence, r.Kind, len(r.Candidates))
		for _, cd := range r.Candidates {
			fmt.Fprintf(w, "  %s:%s %s %d  (%.2f)\n", cd.Provider, cd.ID, cd.Title, cd.Year, cd.Score)
		}
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", warn)
	}
	return nil
}

func runMediaLs(cmd *cobra.Command, args []string) error {
	c, err := newSyncClient()
	if err != nil {
		return err
	}
	for page := 1; ; page++ {
		var resp struct {
			Content []struct {
				Path           string  `json:"path"`
				Kind           string  `json:"kind"`
				Title          string  `json:"title"`
				Year           int     `json:"year"`
				Confidence     float32 `json:"confidence"`
				Locked         bool    `json:"locked"`
				CandidateCount int     `json:"candidate_count"`
			} `json:"content"`
			Total int64 `json:"total"`
		}
		q := url.Values{"prefix": {mediaFlags.prefix}, "page": {strconv.Itoa(page)}, "per_page": {"500"}}
		if mediaFlags.pending {
			q.Set("pending", "true")
		}
		if err := c.Call("GET", "/api/admin/media/list?"+q.Encode(), nil, &resp); err != nil {
			return err
		}
		for _, it := range resp.Content {
			flags := ""
			if it.Locked {
				flags = " locked"
			}
			if it.CandidateCount > 0 {
				flags += fmt.Sprintf(" %d candidates", it.CandidateCount)
			}
			year := ""
			if it.Year > 0 {
				year = " " + strconv.Itoa(it.Year)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%.2f\t%s%s%s\n", it.Path, it.Kind, it.Confidence, strings.TrimSpace(it.Title), year, flags)
		}
		if len(resp.Content) == 0 || int64(page*500) >= resp.Total {
			fmt.Fprintf(os.Stderr, "%d items\n", resp.Total)
			return nil
		}
	}
}
