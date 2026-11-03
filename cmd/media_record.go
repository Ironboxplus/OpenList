package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/bootstrap"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/server/handles"
	"github.com/spf13/cobra"
)

var recordFlags struct {
	names string
	out   string
	max   int
}

// media record keeps real provider answers for the names in a list, as the
// fixture the identification tests replay. It reads the provider settings of the
// data directory exactly like the server (so no key is passed or written
// anywhere), makes requests one at a time, and never lists a storage.
var mediaRecordCmd = &cobra.Command{
	Use:    "record",
	Short:  "Record provider search/detail/season answers for a list of names (fixture for tests)",
	Hidden: true,
	Long: `Reads --names (lines "kind<TAB>name[<TAB>Name:Year;Name:Year]" (the optional names are what the scan found inside the folder), kind anime|tv|movie), identifies each name
with the rules, searches the providers the way a scrape does, fetches the top
3 candidates (and the seasons of a TMDB show) and writes the answers to --out as
JSON lines. The settings (TMDB key, OMDb, Douban, language, proxy, TMDB API URL,
timeout) come from the --data directory; nothing secret is printed or written
(the output is redacted and refused if a secret survives). At most --max-requests
requests go out, strictly one at a time. Touches no storage.

  docker exec openlist ./openlist media record --data /opt/openlist/data \
      --names names.txt --out /opt/openlist/data/temp/rec.jsonl`,
	Args: cobra.NoArgs,
	RunE: runMediaRecord,
}

func init() {
	mediaCmd.AddCommand(mediaRecordCmd)
	mediaRecordCmd.Flags().StringVar(&recordFlags.names, "names", "", "file with lines kind<TAB>name (required)")
	mediaRecordCmd.Flags().StringVar(&recordFlags.out, "out", "", "output .jsonl, must not exist (required)")
	mediaRecordCmd.Flags().IntVar(&recordFlags.max, "max-requests", 400, "stop after this many provider requests")
	_ = mediaRecordCmd.MarkFlagRequired("names")
	_ = mediaRecordCmd.MarkFlagRequired("out")
}

type recordName struct {
	kind, name string
	content    []scraper.ContentName // names found inside the folder, as the scan would give them
}

func readRecordNames(path string) ([]recordName, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []recordName
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kind, name, ok := strings.Cut(line, "\t")
		name, inside, _ := strings.Cut(name, "\t")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("bad line %q: want kind<TAB>name", line)
		}
		switch kind {
		case "anime", "tv", "movie":
		default:
			return nil, fmt.Errorf("bad kind %q in %q", kind, line)
		}
		rn := recordName{kind: kind, name: name}
		for _, c := range strings.Split(inside, ";") {
			if cn, y, ok := strings.Cut(c, ":"); ok && strings.TrimSpace(cn) != "" {
				year, _ := strconv.Atoi(strings.TrimSpace(y))
				rn.content = append(rn.content, scraper.ContentName{Name: strings.TrimSpace(cn), Year: year})
			}
		}
		out = append(out, rn)
	}
	return out, sc.Err()
}

func runMediaRecord(cmd *cobra.Command, args []string) error {
	names, err := readRecordNames(recordFlags.names)
	if err != nil {
		return err
	}
	if _, err := os.Stat(recordFlags.out); err == nil {
		return fmt.Errorf("%s exists; not overwriting", recordFlags.out)
	}
	// Settings only: no storages, no cluster sync, no patches, so this can run next
	// to the live server.
	bootstrap.InitConfig()
	bootstrap.Log()
	bootstrap.InitDB()
	defer db.Close()

	cfg := handles.MediaScraperConfig()
	scraper.Version = conf.Version
	rec, err := scraper.Record(&cfg, recordFlags.max)
	if err != nil {
		return err
	}
	set, err := scraper.NewSet(cfg)
	if err != nil {
		return err
	}
	if set.TMDB == nil {
		return errors.New("no TMDB key in the media settings")
	}

	ctx, stop := mediaContext()
	defer stop()
	rules := identify.Rules{}
	failed := 0
run:
	for i, n := range names {
		if ctx.Err() != nil {
			break
		}
		err := recordOne(ctx, set, rules, n)
		switch {
		case errors.Is(err, scraper.ErrRecordLimit):
			fmt.Fprintf(os.Stderr, "stopped at name %d/%d: request limit\n", i+1, len(names))
			break run
		case err != nil:
			failed++
			fmt.Fprintf(os.Stderr, "name %d/%d: %v\n", i+1, len(names), err)
		}
	}
	werr := rec.WriteFile(recordFlags.out)
	fmt.Fprintf(cmd.OutOrStdout(), "names=%d failed=%d %s out=%s\n", len(names), failed, rec.Summary(), recordFlags.out)
	return werr
}

// recordOne does what ScrapeOne does for an unmatched name: identify, search,
// and fetch the leading candidates.
func recordOne(ctx context.Context, set *scraper.Set, rules identify.Rules, n recordName) error {
	isDir := !identify.IsVideo(n.name)
	r, err := rules.Classify(ctx, n.name, isDir, nil)
	if err != nil {
		return err
	}
	kind := n.kind
	cs, err := set.Identify(ctx, kind, r.Theatrical, scraper.IdentifyQuery{Terms: r.Terms(), Year: r.Year, English: r.English, Content: append(r.Content, n.content...)})
	if errors.Is(err, scraper.ErrRecordLimit) {
		return err
	}
	if len(cs) > 3 {
		cs = cs[:3]
	}
	for _, c := range cs {
		tg, _, ferr := set.Fetch(ctx, c.Provider, c.ID, kind)
		if errors.Is(ferr, scraper.ErrRecordLimit) {
			return ferr
		}
		if ferr != nil || tg == nil {
			continue
		}
		if _, eerr := set.Episodes(ctx, tg); errors.Is(eerr, scraper.ErrRecordLimit) {
			return eerr
		}
	}
	return err
}
