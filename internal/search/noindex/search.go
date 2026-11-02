package noindex

import (
	"context"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/search/searcher"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/pkg/errors"
)

// maxResults caps how many matches a single live search collects, to bound the
// cost of walking large storages without an index.
var maxResults = 1000

// Defaults of the limits below. They are our own conservative values; AList only
// caps the number of results.
const (
	defaultMaxVisited    = 5000
	defaultTimeoutSecond = 20
	defaultMaxConcurrent = 2
	defaultSkipDrivers   = "115 Cloud,115 Open,115 Share"
)

// errStop aborts the walk once enough matches have been collected or a limit is hit.
var errStop = errors.New("enough results collected")

// ErrBusy is returned when too many live searches are already running.
var ErrBusy = errors.New("too many live searches are running, try again later")

// running is the number of live searches in progress.
var running atomic.Int32

// NoIndex is an index-free searcher: instead of querying a prebuilt index it
// walks the filesystem under the requested parent on every search and matches
// object names against the keywords. It trades query speed for not having to
// build and maintain an index. See AList issue #9468.
type NoIndex struct{}

// limits bound the work of one live search.
type limits struct {
	maxDepth      int
	maxVisited    int           // entries listed and looked at
	timeout       time.Duration // for the whole walk
	maxConcurrent int           // live searches at the same time
	skipDrivers   []string      // driver names that are never walked
	ignorePaths   []string
}

func loadLimits() limits {
	positive := func(key string, def int) int {
		if v := setting.GetInt(key, def); v > 0 {
			return v
		}
		return def
	}
	return limits{
		maxDepth:      setting.GetInt(conf.MaxIndexDepth, 20),
		maxVisited:    positive(conf.NoIndexMaxVisited, defaultMaxVisited),
		timeout:       time.Duration(positive(conf.NoIndexTimeout, defaultTimeoutSecond)) * time.Second,
		maxConcurrent: positive(conf.NoIndexMaxConcurrent, defaultMaxConcurrent),
		skipDrivers:   splitList(setting.GetStr(conf.NoIndexSkipDrivers, defaultSkipDrivers)),
		ignorePaths:   conf.SlicesMap[conf.IgnorePaths],
	}
}

// splitList splits a comma separated setting, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func skipDriver(name string, list []string) bool {
	for _, v := range list {
		if strings.EqualFold(v, name) {
			return true
		}
	}
	return false
}

// skipStorage reports whether the storage serving reqPath must not be walked:
// it has indexing disabled, or its driver is on the skip list.
func (l limits) skipStorage(reqPath string) bool {
	storage, _, err := op.GetStorageAndActualPath(reqPath)
	if err != nil {
		return false
	}
	return storage.GetStorage().DisableIndex || skipDriver(storage.Config().Name, l.skipDrivers)
}

func (NoIndex) Config() searcher.Config {
	return config
}

func (n NoIndex) Search(ctx context.Context, req model.SearchReq) ([]model.SearchNode, int64, error) {
	return n.SearchFiltered(ctx, req, nil)
}

// SearchFiltered applies filter to every candidate before it counts towards the
// result cap, so entries the caller may not see cannot crowd out those it may.
func (NoIndex) SearchFiltered(ctx context.Context, req model.SearchReq, filter searcher.Filter) ([]model.SearchNode, int64, error) {
	return search(ctx, req, filter, loadLimits())
}

func search(ctx context.Context, req model.SearchReq, filter searcher.Filter, lim limits) ([]model.SearchNode, int64, error) {
	if running.Add(1) > int32(lim.maxConcurrent) {
		running.Add(-1)
		return nil, 0, ErrBusy
	}
	defer running.Add(-1)

	keywords := strings.Fields(req.Keywords)
	parent := req.Parent
	rootObj, err := fs.Get(ctx, parent, &fs.GetArgs{NoLog: true})
	if err != nil {
		return nil, 0, errors.WithMessagef(err, "failed get dir [%s]", parent)
	}

	walkCtx, cancel := context.WithTimeout(ctx, lim.timeout)
	defer cancel()

	var nodes []model.SearchNode
	visited := 0
	walkFn := func(reqPath string, info model.Obj) error {
		if walkCtx.Err() != nil {
			return errStop
		}
		visited++
		if visited > lim.maxVisited {
			return errStop
		}
		if info.IsDir() && lim.skipStorage(reqPath) {
			return filepath.SkipDir
		}
		// the parent itself is not a search result
		if reqPath == parent {
			return nil
		}
		for _, ignore := range lim.ignorePaths {
			if strings.HasPrefix(reqPath, ignore) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		// scope: 0 for all, 1 for dir, 2 for file
		if (req.Scope == 1 && !info.IsDir()) || (req.Scope == 2 && info.IsDir()) {
			return nil
		}
		if !matchKeywords(info.GetName(), keywords) {
			return nil
		}
		node := model.SearchNode{
			Parent: path.Dir(reqPath),
			Name:   info.GetName(),
			IsDir:  info.IsDir(),
			Size:   info.GetSize(),
		}
		if filter != nil && !filter(node) {
			return nil
		}
		nodes = append(nodes, node)
		if len(nodes) >= maxResults {
			return errStop
		}
		return nil
	}
	if err := fs.WalkFS(walkCtx, lim.maxDepth, parent, rootObj, walkFn); err != nil && !errors.Is(err, errStop) {
		return nil, 0, err
	}

	// stable ordering so pagination is consistent across repeated calls
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Name != nodes[j].Name {
			return nodes[i].Name < nodes[j].Name
		}
		return nodes[i].Parent < nodes[j].Parent
	})

	total := int64(len(nodes))
	start := (req.Page - 1) * req.PerPage
	if start < 0 || start >= len(nodes) {
		return []model.SearchNode{}, total, nil
	}
	end := start + req.PerPage
	if end > len(nodes) {
		end = len(nodes)
	}
	return nodes[start:end], total, nil
}

// matchKeywords reports whether name contains every keyword (case-insensitive),
// matching the AND semantics of the indexed searchers. Empty keywords match all.
func matchKeywords(name string, keywords []string) bool {
	lower := strings.ToLower(name)
	for _, kw := range keywords {
		if !strings.Contains(lower, strings.ToLower(kw)) {
			return false
		}
	}
	return true
}

func (NoIndex) Index(ctx context.Context, node model.SearchNode) error {
	return nil
}

func (NoIndex) BatchIndex(ctx context.Context, nodes []model.SearchNode) error {
	return nil
}

func (NoIndex) Get(ctx context.Context, parent string) ([]model.SearchNode, error) {
	return []model.SearchNode{}, nil
}

func (NoIndex) Del(ctx context.Context, prefix string) error {
	return nil
}

func (NoIndex) Release(ctx context.Context) error {
	return nil
}

func (NoIndex) Clear(ctx context.Context) error {
	return nil
}

var (
	_ searcher.Searcher         = (*NoIndex)(nil)
	_ searcher.FilteredSearcher = (*NoIndex)(nil)
)
