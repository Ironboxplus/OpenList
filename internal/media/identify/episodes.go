package identify

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// EpisodeFile is a video file of a show with the season and episode its name
// (or its season directory) carries.
type EpisodeFile struct {
	Rel     string // relative to the show's directory, e.g. "Season 1/x.S01E03.mkv"
	Name    string
	Season  int
	Episode int
	// Version marks a copy of an episode another file already provides (the
	// same season and number in another release folder): a version of that
	// episode, not another episode.
	Version bool
}

// fileRef is a candidate episode file while the show is being read.
type fileRef struct {
	rel, name string
	dirSeason int
	special   int // 1-based number of a special (season 0), 0 for episodes
	version   bool
}

// MaxEpisodeFiles caps what EpisodeFiles returns.
const MaxEpisodeFiles = 3000

// maxSeasonDirs caps how many season directories are opened.
const maxSeasonDirs = 30

// EpisodeFiles lists the episode-looking videos of a show: those directly in
// its directory and those in its season directories, one level down. list
// returns the entries of the directory rel (relative to the show; "" is the
// show itself). A file with no season of its own takes its directory's season,
// else season 1. Files without an episode marker are left out.
func EpisodeFiles(list func(rel string) ([]Entry, error)) ([]EpisodeFile, error) {
	// 115 keeps every file in a directory of its own name: one level is unwrapped
	// wherever a listing is read, from one budget of directories to open.
	list = unwrapping(list)
	top, err := list("")
	if err != nil {
		return nil, err
	}
	var files []fileRef
	var subdirs []Entry
	for _, e := range top {
		switch {
		case e.IsDir:
			if !IsExtraDir(e.Name) {
				subdirs = append(subdirs, e)
			}
		case IsVideo(e.Name):
			files = append(files, fileRef{rel: e.Rel(), name: e.Name})
		}
	}
	sort.Slice(subdirs, func(i, j int) bool { return subdirs[i].Name < subdirs[j].Name })
	var specialDirs []Entry
	seasonDirs := 0
	for _, d := range subdirs {
		if IsSeasonDir(d.Name) {
			seasonDirs++
		} else {
			specialDirs = append(specialDirs, d)
		}
	}
	for i, d := range subdirs {
		if i >= maxSeasonDirs {
			break
		}
		if !IsSeasonDir(d.Name) {
			continue
		}
		kids, err := list(d.Name)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", d.Name, err)
		}
		for _, k := range kids {
			if !k.IsDir && IsVideo(k.Name) {
				files = append(files, fileRef{rel: path.Join(d.Name, k.Rel()), name: k.Name, dirSeason: seasonOfDirName(d.Name)})
			}
		}
	}
	// No season folders and no episodes of its own: the subfolders are copies or
	// seasons under other names (魔道祖师: two releases, 星蝶公主: two languages,
	// each holding its seasons in folders of its own, one of them wrapped once
	// more). A folder that names its season keeps it; of the others the one with
	// most episodes is the show; the others add the episodes it lacks and are
	// kept as versions of those it has (a file with no season of its own and no
	// folder naming one is left out).
	if seasonDirs == 0 && len(files) == 0 {
		type variant struct {
			refs   []fileRef
			season int
		}
		var named, unnamed []variant
		numbered := siblingSeasons(specialDirs)
		for i, d := range specialDirs {
			if i >= maxSeasonDirs {
				break
			}
			refs, err := variantRefs(list, d.Name, 0)
			if err != nil {
				return nil, err
			}
			v := variant{refs: refs, season: seasonOfDirName(d.Name)}
			if v.season == 0 {
				v.season = numbered[d.Name]
			}
			for j := range v.refs {
				if v.refs[j].dirSeason == 0 {
					v.refs[j].dirSeason = v.season
				}
			}
			switch {
			case len(v.refs) == 0:
			case v.season > 0:
				named = append(named, v)
			default:
				unnamed = append(unnamed, v)
			}
		}
		sort.SliceStable(unnamed, func(i, j int) bool { return len(unnamed[i].refs) > len(unnamed[j].refs) })
		taken := map[[2]int]bool{}
		takenSeason := map[int]bool{}
		keyOf := func(r fileRef, fallback int) (int, int, bool) {
			s, e, ok := EpisodeOf(r.name)
			if !ok {
				return 0, 0, false
			}
			if s == 0 {
				s = r.dirSeason
			}
			if s == 0 {
				s = fallback
			}
			return s, e, s > 0
		}
		for _, v := range named {
			takenSeason[v.season] = true
			for _, r := range v.refs {
				if s, e, ok := keyOf(r, 0); ok {
					taken[[2]int{s, e}] = true
				}
				files = append(files, r)
			}
		}
		for i, v := range unnamed {
			fallback := 0
			if i == 0 {
				fallback = 1
				for takenSeason[fallback] {
					fallback++
				}
			}
			for _, r := range v.refs {
				s, e, ok := keyOf(r, fallback)
				if !ok {
					continue
				}
				r.version = taken[[2]int{s, e}]
				taken[[2]int{s, e}] = true
				if r.dirSeason == 0 {
					r.dirSeason = s
				}
				files = append(files, r)
			}
		}
	}
	// A directory next to the seasons holding one video without an episode
	// marker is a film of the show (续命之徒 beside 绝命毒师's five seasons): a
	// special, season 0, as Jellyfin files it.
	specials := 0
	if seasonDirs > 0 {
		for i, d := range specialDirs {
			if i >= maxSeasonDirs {
				break
			}
			kids, err := list(d.Name)
			if err != nil {
				return nil, fmt.Errorf("list %s: %w", d.Name, err)
			}
			var vs []Entry
			for _, k := range kids {
				if !k.IsDir && IsVideo(k.Name) {
					vs = append(vs, k)
				}
			}
			if vs = mainVideos(vs); len(vs) == 1 {
				if _, _, marked := EpisodeOf(vs[0].Name); !marked {
					specials++
					files = append(files, fileRef{rel: path.Join(d.Name, vs[0].Rel()), name: vs[0].Name, special: specials})
				}
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	var out []EpisodeFile
	for _, f := range files {
		if f.special > 0 {
			out = append(out, EpisodeFile{Rel: f.rel, Name: f.name, Season: 0, Episode: f.special})
			continue
		}
		s, e, ok := EpisodeOf(f.name)
		if !ok {
			continue
		}
		if s == 0 {
			s = f.dirSeason
		}
		if s == 0 {
			s = 1
		}
		out = append(out, EpisodeFile{Rel: f.rel, Name: f.name, Season: s, Episode: e, Version: f.version})
		if len(out) >= MaxEpisodeFiles {
			break
		}
	}
	return out, nil
}

// maxVariantDepth is how many levels below a release folder are read for
// episodes (the folder, a wrapper, the season folders in it).
const maxVariantDepth = 2

// variantRefs collects the episode-marked videos under one release folder of a
// show: those in it and in the folders below it, down to maxVariantDepth. A
// file takes the season of the nearest folder that names one, else 0 (its own
// name may carry it).
func variantRefs(list func(rel string) ([]Entry, error), rel string, depth int) ([]fileRef, error) {
	kids, err := list(rel)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", rel, err)
	}
	var refs []fileRef
	var dirs []Entry
	for _, k := range kids {
		switch {
		case k.IsDir:
			if !IsExtraDir(k.Name) {
				dirs = append(dirs, k)
			}
		case IsVideo(k.Name):
			if _, _, ok := EpisodeOf(k.Name); ok {
				refs = append(refs, fileRef{rel: path.Join(rel, k.Rel()), name: k.Name})
			}
		}
	}
	if depth >= maxVariantDepth {
		return refs, nil
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	for i, d := range dirs {
		if i >= maxSeasonDirs {
			break
		}
		sub, err := variantRefs(list, path.Join(rel, d.Name), depth+1)
		if err != nil {
			return nil, err
		}
		season := seasonOfDirName(d.Name)
		for _, r := range sub {
			if r.dirSeason == 0 {
				r.dirSeason = season
			}
			refs = append(refs, r)
		}
	}
	return refs, nil
}

// unwrapping makes list return its entries with UnwrapEntries applied. The
// budget of directories to open is shared by all the listings of one call.
func unwrapping(list func(rel string) ([]Entry, error)) func(rel string) ([]Entry, error) {
	budget := maxUnwrap
	return func(rel string) ([]Entry, error) {
		es, err := list(rel)
		if err != nil || budget <= 0 {
			return es, err
		}
		var opened int
		out := UnwrapEntries(es, func(name string) ([]Entry, error) {
			opened++
			return list(path.Join(rel, name))
		})
		budget -= opened
		return out, nil
	}
}

// SiblingSeasons is siblingSeasons for callers outside the package.
func SiblingSeasons(dirs []Entry) map[string]int { return siblingSeasons(dirs) }

// siblingSeasons reads the seasons of directories that are one name with a
// number added ("Log Horizon", "Log Horizon 2"): the bare name is season 1 and
// the numbered ones are the seasons they say. Release tags in brackets do not
// count. Anything else (two copies of one name, a different word) gives nil: the
// directories are not seasons of each other by their names.
func siblingSeasons(dirs []Entry) map[string]int {
	if len(dirs) < 2 {
		return nil
	}
	norm := make([]string, len(dirs))
	base := -1
	for i, d := range dirs {
		norm[i] = strings.ToLower(strings.Join(strings.Fields(bracketGroupRe.ReplaceAllString(d.Name, " ")), " "))
		if norm[i] == "" {
			return nil
		}
		if base < 0 || len(norm[i]) < len(norm[base]) {
			base = i
		}
	}
	out := map[string]int{}
	used := map[int]bool{}
	for i, d := range dirs {
		if i == base {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(norm[i], norm[base]))
		n, err := strconv.Atoi(rest)
		if !strings.HasPrefix(norm[i], norm[base]) || err != nil || n < 2 || n > 30 || used[n] || len(rest) > 2 {
			return nil
		}
		used[n] = true
		out[d.Name] = n
	}
	out[dirs[base].Name] = 1
	return out
}

// SameTitleSiblings reports whether directories differ only in their bracketed
// tags: copies of one show released by different groups, not different seasons.
func SameTitleSiblings(dirs []Entry) bool {
	for i, d := range dirs {
		n := strings.ToLower(strings.Join(strings.Fields(bracketGroupRe.ReplaceAllString(d.Name, " ")), " "))
		m := strings.ToLower(strings.Join(strings.Fields(bracketGroupRe.ReplaceAllString(dirs[0].Name, " ")), " "))
		if i > 0 && n != m {
			return false
		}
	}
	return len(dirs) > 1
}
