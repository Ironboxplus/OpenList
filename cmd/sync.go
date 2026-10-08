package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/bootstrap"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/syncjob"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The sync commands are a thin client: the job runs inside the server (see
// internal/syncjob for why), the CLI starts it and follows its progress.

var syncFlags struct {
	server       string
	token        string
	tokenFile    string
	dryRun       bool
	sizeOnly     bool
	trackRenames bool
	resync       bool
	force        bool
	filters      []string
	detach       bool
	transfers    int
	checkers     int
	maxDelete    int64
	events       bool
	limit        int
	follow       bool

	pack          bool
	packThreshold string
	packMinFiles  int
	packMax       string
	unpack        bool
	unpackSync    bool
}

const endpointHelp = `
Endpoints:
  /mount/path        a path in OpenList (any mounted storage)
  local:/abs/dir     a directory on the machine the server runs on
  ./dir, D:\dir      a local directory; made absolute against the current dir

Files are compared by size and hash (cloud drives can't keep modification
times). Uploads to drives with rapid upload (115 etc.) check the hash first
and only transfer what the drive doesn't already have.

The server must be running; the job runs inside it. Follow or inspect it with
"openlist jobs", stop it with "openlist jobs cancel", and cap the bandwidth of
all running jobs with "openlist bwlimit". Ctrl-C only stops following; the
job keeps running until it ends or "openlist jobs cancel <id>".

copy and sync can pack small files (--pack) so a drive with a per-file API
cost sees a few large uploads instead of thousands of tiny ones, and unpack
them again (--unpack, or "openlist sync unpack").`

func newSyncCommand(mode syncjob.Mode, use, short string) *cobra.Command {
	c := &cobra.Command{
		Use:   use + " <src> <dst>",
		Short: short,
		Long:  short + "\n" + endpointHelp,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(cmd, mode, args)
		},
	}
	f := c.Flags()
	addCommonFlags(f)
	switch mode {
	case syncjob.ModeCopy:
		f.BoolVar(&syncFlags.trackRenames, "track-renames", false, "turn renames at src into server-side moves at dst instead of re-uploads")
		addPackFlags(f)
	case syncjob.ModeSync:
		f.BoolVar(&syncFlags.trackRenames, "track-renames", false, "turn renames at src into server-side moves at dst instead of re-uploads")
		f.Int64Var(&syncFlags.maxDelete, "max-delete", 0, "abort if more than this many files would be deleted at dst")
		addPackFlags(f)
		c.AddCommand(newUnpackCommand())
	case syncjob.ModeBisync:
		f.BoolVar(&syncFlags.resync, "resync", false, "rebuild the baseline; required on the first run")
		f.BoolVar(&syncFlags.force, "force", false, "allow a run that deletes more than half the files")
	}
	return c
}

func addCommonFlags(f *pflag.FlagSet) {
	f.BoolVarP(&syncFlags.dryRun, "dry-run", "n", false, "show what would change without changing anything")
	f.BoolVar(&syncFlags.sizeOnly, "size-only", false, "compare by size only (skips hashing; misses same-size edits)")
	f.StringArrayVar(&syncFlags.filters, "filter", nil, `rclone filter rule, repeatable, e.g. --filter "- *.tmp"`)
	f.BoolVarP(&syncFlags.detach, "detach", "d", false, "start the job and return without waiting")
	f.IntVar(&syncFlags.transfers, "transfers", 0, "files transferred at once (default 4)")
	f.IntVar(&syncFlags.checkers, "checkers", 0, "files compared at once (default 8)")
	addServerFlags(f)
}

func runSync(cmd *cobra.Command, mode syncjob.Mode, args []string) error {
	src, err := endpointArg(args[0])
	if err != nil {
		return err
	}
	dst, err := endpointArg(args[1])
	if err != nil {
		return err
	}
	spec := syncjob.Spec{
		Mode:         mode,
		Src:          src,
		Dst:          dst,
		DryRun:       syncFlags.dryRun,
		SizeOnly:     syncFlags.sizeOnly,
		TrackRenames: syncFlags.trackRenames,
		Resync:       syncFlags.resync,
		Force:        syncFlags.force,
		Filters:      syncFlags.filters,
		Transfers:    syncFlags.transfers,
		Checkers:     syncFlags.checkers,
	}
	if cmd.Flags().Changed("max-delete") {
		spec.MaxDelete = &syncFlags.maxDelete
	}
	if err := applyPackFlags(&spec); err != nil {
		return err
	}
	client, err := newSyncClient()
	if err != nil {
		return err
	}
	snap, err := client.start(spec)
	if err != nil {
		return err
	}
	fmt.Printf("job %s started: %s %s -> %s\n", snap.ID, mode, src, dst)
	if syncFlags.detach {
		return nil
	}
	return client.follow(interruptContext(), snap.ID)
}

func addPackFlags(f *pflag.FlagSet) {
	d := syncjob.DefaultPackOptions()
	f.BoolVar(&syncFlags.pack, "pack", false, "zip each directory's small files into packs at dst")
	f.StringVar(&syncFlags.packThreshold, "pack-threshold", formatSize(d.Threshold), "with --pack: files smaller than this are packed (K, M, G suffixes)")
	f.IntVar(&syncFlags.packMinFiles, "pack-min-files", d.MinFiles, "with --pack: don't pack a directory with fewer small files than this")
	f.StringVar(&syncFlags.packMax, "pack-max", formatSize(d.MaxPackBytes), "with --pack: largest pack; more content goes into more packs (K, M, G suffixes)")
	f.BoolVar(&syncFlags.unpack, "unpack", false, "extract packs (and legacy PACKED.tsv archives) found at src into plain files at dst")
}

func newUnpackCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "unpack <src> <dst>",
		Short: "Extract packed directories at src into plain files at dst (copy --unpack; with --sync, sync --unpack)",
		Long: "Extract packed directories at src into plain files at dst. Same as \"openlist copy --unpack\",\n" +
			"or with --sync \"openlist sync --unpack\", which also deletes files at dst that are not at src.\n" + endpointHelp,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := syncjob.ModeCopy
			if syncFlags.unpackSync {
				mode = syncjob.ModeSync
			}
			syncFlags.unpack = true
			return runSync(cmd, mode, args)
		},
	}
	f := c.Flags()
	addCommonFlags(f)
	f.BoolVar(&syncFlags.unpackSync, "sync", false, "also delete files at dst that are not at src")
	f.Int64Var(&syncFlags.maxDelete, "max-delete", 0, "with --sync: abort if more than this many files would be deleted at dst")
	return c
}

// applyPackFlags copies the pack flags into spec, rejecting contradictions
// before anything is sent.
func applyPackFlags(spec *syncjob.Spec) error {
	if syncFlags.pack && syncFlags.unpack {
		return fmt.Errorf("--pack and --unpack can't be used together")
	}
	spec.Unpack = syncFlags.unpack
	if !syncFlags.pack {
		return nil
	}
	threshold, err := parseSize(syncFlags.packThreshold)
	if err != nil {
		return fmt.Errorf("--pack-threshold: %w", err)
	}
	maxPack, err := parseSize(syncFlags.packMax)
	if err != nil {
		return fmt.Errorf("--pack-max: %w", err)
	}
	if threshold <= 0 || maxPack <= 0 || syncFlags.packMinFiles <= 0 {
		return fmt.Errorf("--pack-threshold, --pack-min-files and --pack-max must be positive")
	}
	spec.Pack = &syncjob.PackOptions{Enabled: true, Threshold: threshold, MinFiles: syncFlags.packMinFiles, MaxPackBytes: maxPack}
	return nil
}

// parseSize reads a byte count with an optional K, M, G or T suffix (powers of
// 1024; "KiB" and "KB" spellings are accepted too).
func parseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(strings.TrimSuffix(t, "IB"), "B")
	mult := int64(1)
	if t != "" {
		if i := strings.IndexByte("KMGT", t[len(t)-1]); i >= 0 {
			mult = int64(1) << (10 * (i + 1))
			t = t[:len(t)-1]
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q (want e.g. 512K, 8M, 1G)", s)
	}
	return int64(n * float64(mult)), nil
}

// formatSize is parseSize's inverse for round numbers: 8388608 -> "8M".
func formatSize(n int64) string {
	for i, suffix := range []string{"T", "G", "M", "K"} {
		unit := int64(1) << (10 * (4 - i))
		if n >= unit && n%unit == 0 {
			return fmt.Sprintf("%d%s", n/unit, suffix)
		}
	}
	return strconv.FormatInt(n, 10)
}

var jobsCmd = &cobra.Command{
	Use:   "jobs [id]",
	Short: "List sync jobs, or show one (--follow to watch it, --events for its per-file log)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newSyncClient()
		if err != nil {
			return err
		}
		if len(args) == 1 {
			if syncFlags.events {
				return client.printEvents(args[0])
			}
			if syncFlags.follow {
				return client.follow(interruptContext(), args[0])
			}
			return client.show(os.Stdout, args[0])
		}
		var list []syncjob.Snapshot
		if err := client.call(http.MethodGet, "/api/admin/sync/list", nil, &list); err != nil {
			return err
		}
		printJobs(list)
		return nil
	},
}

var jobsHistoryCmd = &cobra.Command{
	Use:   "history",
	Short: "Show finished sync jobs, including those from before a restart",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newSyncClient()
		if err != nil {
			return err
		}
		var list []syncjob.Snapshot
		if err := client.call(http.MethodGet, fmt.Sprintf("/api/admin/sync/history?limit=%d", syncFlags.limit), nil, &list); err != nil {
			return err
		}
		printJobs(list)
		return nil
	},
}

var jobsCancelCmd = &cobra.Command{
	Use:   "cancel <id>",
	Short: "Cancel a running sync job",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newSyncClient()
		if err != nil {
			return err
		}
		return client.call(http.MethodPost, "/api/admin/sync/cancel?id="+url.QueryEscape(args[0]), nil, nil)
	},
}

var bwlimitCmd = &cobra.Command{
	Use:   "bwlimit [rate]",
	Short: `Show or set the bandwidth cap shared by all sync jobs ("10M", "10M:2M" up:down, "off")`,
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newSyncClient()
		if err != nil {
			return err
		}
		var out struct {
			BytesPerSecond int64 `json:"bytes_per_second"`
		}
		if len(args) == 1 {
			err = client.call(http.MethodPost, "/api/admin/sync/bwlimit", map[string]string{"rate": args[0]}, &out)
		} else {
			err = client.call(http.MethodGet, "/api/admin/sync/bwlimit", nil, &out)
		}
		if err != nil {
			return err
		}
		if out.BytesPerSecond < 0 {
			fmt.Println("sync bandwidth: unlimited")
		} else {
			fmt.Printf("sync bandwidth: %s/s\n", humanBytes(float64(out.BytesPerSecond)))
		}
		return nil
	},
}

func init() {
	RootCmd.AddCommand(
		newSyncCommand(syncjob.ModeCopy, "copy", "Copy new and changed files from src to dst, never deleting"),
		newSyncCommand(syncjob.ModeSync, "sync", "Make dst identical to src, deleting files only at dst"),
		newSyncCommand(syncjob.ModeBisync, "bisync", "Two-way sync: propagate changes on either side to the other"),
		newSyncCommand(syncjob.ModeCheck, "check", "Compare src and dst and report differences"),
		jobsCmd,
		bwlimitCmd,
	)
	jobsCmd.AddCommand(jobsCancelCmd, jobsHistoryCmd)
	jobsCmd.Flags().BoolVar(&syncFlags.events, "events", false, "print the job's per-file log")
	jobsCmd.Flags().BoolVar(&syncFlags.follow, "follow", false, "keep printing progress until the job ends (Ctrl-C stops following, not the job)")
	jobsHistoryCmd.Flags().IntVar(&syncFlags.limit, "limit", 20, "how many jobs to show")
	addServerFlags(jobsCmd.PersistentFlags())
	addServerFlags(bwlimitCmd.Flags())
	// rclone's command package (pulled in for bisync) registers its own
	// process flags on the global flag set, which cobra then lists under
	// every OpenList command. None of them mean anything here.
	pflag.CommandLine.VisitAll(func(f *pflag.Flag) { f.Hidden = true })
}

func printJobs(list []syncjob.Snapshot) {
	for _, s := range list {
		c := s.Counters
		fmt.Printf("%s  %-9s  %-6s %s -> %s  started %s  %d uploaded (%d rapid), %d errors\n",
			s.ID, s.State, s.Spec.Mode, s.Spec.Src, s.Spec.Dst, s.StartedAt.Local().Format(time.DateTime),
			c.Uploaded, c.RapidUploads, c.Errors)
		if s.Error != "" {
			fmt.Printf("    error: %s\n", s.Error)
		}
	}
}

func (c *syncClient) printEvents(id string) error {
	var s syncjob.Snapshot
	if err := c.call(http.MethodGet, "/api/admin/sync/get?events=true&id="+url.QueryEscape(id), nil, &s); err != nil {
		return err
	}
	for _, e := range s.Events {
		line := fmt.Sprintf("%s  %-11s %s", e.Time.Local().Format(time.TimeOnly), e.Kind, e.Path)
		if e.Size > 0 {
			line += "  " + humanBytes(float64(e.Size))
		}
		if e.Error != "" {
			line += "  " + e.Error
		}
		fmt.Println(line)
	}
	fmt.Println(summaryLine(s))
	return nil
}

func addServerFlags(f *pflag.FlagSet) {
	f.StringVar(&syncFlags.server, "server", "", "server base URL (default: this machine's server, from its config)")
	f.StringVar(&syncFlags.token, "token", "", "admin token (default: read from the data directory)")
	f.StringVar(&syncFlags.tokenFile, "token-file", "", "read the admin token from this file instead of --token")
}

// endpointArg normalises a command-line endpoint into the server's syntax:
// "/x" stays an OpenList path, everything else is a local directory made
// absolute here, against the directory the user ran the command in. On
// Linux an absolute local directory therefore needs the local: prefix.
func endpointArg(arg string) (string, error) {
	if strings.HasPrefix(arg, "/") {
		return arg, nil
	}
	dir := strings.TrimPrefix(arg, syncjob.LocalPrefix)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("local path %q: %w", dir, err)
	}
	return syncjob.LocalPrefix + abs, nil
}

type syncClient struct {
	base  string
	token string
	http  *http.Client
}

func newSyncClient() (*syncClient, error) {
	base, token := syncFlags.server, syncFlags.token
	if token == "" && syncFlags.tokenFile != "" {
		b, err := os.ReadFile(syncFlags.tokenFile)
		if err != nil {
			return nil, fmt.Errorf("read --token-file: %w", err)
		}
		if token = strings.TrimSpace(string(b)); token == "" {
			return nil, fmt.Errorf("--token-file %s is empty", syncFlags.tokenFile)
		}
	}
	if base == "" || token == "" {
		bootstrap.InitConfig()
		if base == "" {
			base = localServerURL()
		}
		if token == "" {
			bootstrap.InitDB()
			defer db.Close()
			token = setting.GetStr(conf.Token)
			if token == "" {
				return nil, fmt.Errorf("no admin token found in the data directory; pass --token")
			}
		}
	}
	return &syncClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http: &http.Client{Transport: &http.Transport{
			// Our own server on loopback, possibly with a self-signed cert.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: strings.Contains(base, "127.0.0.1")},
		}},
	}, nil
}

func localServerURL() string {
	s := conf.Conf.Scheme
	prefix := ""
	if conf.URL != nil {
		prefix = strings.TrimRight(conf.URL.Path, "/")
	}
	if s.HttpPort > 0 {
		return fmt.Sprintf("http://127.0.0.1:%d%s", s.HttpPort, prefix)
	}
	return fmt.Sprintf("https://127.0.0.1:%d%s", s.HttpsPort, prefix)
}

func (c *syncClient) start(spec syncjob.Spec) (syncjob.Snapshot, error) {
	var snap syncjob.Snapshot
	err := c.call(http.MethodPost, "/api/admin/sync/run", spec, &snap)
	return snap, err
}

func (c *syncClient) call(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.roundTrip(req, method, path, out)
}

// Call and Upload let the media commands use this client.
func (c *syncClient) Call(method, path string, body, out any) error {
	return c.call(method, path, body, out)
}

// Upload posts data as a multipart form file.
func (c *syncClient) Upload(path, field, filename string, data []byte, out any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.roundTrip(req, http.MethodPost, path, out)
}

// roundTrip sends req with the admin token and unwraps the {code, message,
// data} envelope.
func (c *syncClient) roundTrip(req *http.Request, method, path string, out any) error {
	req.Header.Set("Authorization", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach the OpenList server at %s (is it running?): %w", c.base, err)
	}
	defer resp.Body.Close()
	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("%s %s: HTTP %d, unreadable response: %w", method, path, resp.StatusCode, err)
	}
	if env.Code != 200 {
		return fmt.Errorf("%s", env.Message)
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// interruptContext is cancelled by Ctrl-C.
func interruptContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt)
	return ctx
}

// show prints a job's current state once.
func (c *syncClient) show(w io.Writer, id string) error {
	var s syncjob.Snapshot
	if err := c.call(http.MethodGet, "/api/admin/sync/get?id="+url.QueryEscape(id), nil, &s); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s  %s %s -> %s  started %s\n", s.ID, s.Spec.Mode, s.Spec.Src, s.Spec.Dst, s.StartedAt.Local().Format(time.DateTime))
	fmt.Fprintln(w, progressLine(s))
	if pl := packLine(s); pl != "" {
		fmt.Fprintln(w, pl)
	}
	fmt.Fprintln(w, summaryLine(s))
	if s.Error != "" {
		fmt.Fprintf(w, "error: %s\n", s.Error)
	}
	return nil
}

// follow prints progress until the job ends. Ctrl-C (ctx cancelled) only
// stops following: a job is cancelled explicitly with "jobs cancel".
func (c *syncClient) follow(ctx context.Context, id string) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		var s syncjob.Snapshot
		if err := c.call(http.MethodGet, "/api/admin/sync/get?id="+url.QueryEscape(id), nil, &s); err != nil {
			return err
		}
		fmt.Printf("\r%s", progressLine(s))
		if s.State != syncjob.StateRunning {
			fmt.Println()
			if pl := packLine(s); pl != "" {
				fmt.Println(pl)
			}
			fmt.Println(summaryLine(s))
			if s.State != syncjob.StateSucceeded {
				return fmt.Errorf("job %s %s: %s", id, s.State, s.Error)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Printf("\nstopped following; job %s keeps running (cancel it with: openlist jobs cancel %s)\n", id, id)
			return nil
		case <-tick.C:
		}
	}
}

// packLine summarises packing / unpacking progress, "" for jobs that do neither.
func packLine(s syncjob.Snapshot) string {
	p := s.PackStats
	if p == nil {
		return ""
	}
	if s.Spec.Unpack {
		return fmt.Sprintf("unpack: %d dirs, %d files extracted (%s), %d packs fetched, %d read by range",
			p.Dirs, p.UnpackedFiles, humanBytes(float64(p.UnpackedBytes)), p.PacksFetched, p.PacksRanged)
	}
	return fmt.Sprintf("pack: %d dirs, %d files -> %d packs (%s read, %s written), %d dirs too small to pack, %d entries retired",
		p.Dirs, p.PackedFiles, p.Packs, humanBytes(float64(p.BytesIn)), humanBytes(float64(p.BytesOut)), p.SkippedDirs, p.DeadRemoved)
}

func progressLine(s syncjob.Snapshot) string {
	num := func(k string) float64 {
		v, _ := s.Stats[k].(float64)
		return v
	}
	state := string(s.State)
	if s.State == syncjob.StateRunning && s.Stage != "" {
		state = s.Stage
	}
	return fmt.Sprintf("%-12s %s / %s  %.0f/%.0f files  %.0f checks  %.0f deleted  %.0f renamed  %.0f errors  %s/s   ",
		state,
		humanBytes(num("bytes")), humanBytes(num("totalBytes")),
		num("transfers"), num("totalTransfers"), num("checks"),
		num("deletes"), num("renames"), num("errors"),
		humanBytes(num("speed")))
}

func summaryLine(s syncjob.Snapshot) string {
	c := s.Counters
	return fmt.Sprintf("%s: %d uploaded (%s), %d rapid uploads (%s not transferred), %d differ, %d only at src, %d only at dst, %d unchanged, %d errors",
		s.State, c.Uploaded-c.RapidUploads, humanBytes(float64(c.UploadedBytes)), c.RapidUploads, humanBytes(float64(c.RapidBytes)),
		c.Differ, c.OnlyInSrc, c.OnlyInDst, c.Match, c.Errors)
}

func humanBytes(b float64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%.0f B", b)
	}
	div, exp := float64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", b/div, "KMGTPE"[exp])
}
