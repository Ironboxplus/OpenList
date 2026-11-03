package identify

import (
	"fmt"
	"path"
	"sort"
)

// EpisodeFile is a video file of a show with the season and episode its name
// (or its season directory) carries.
type EpisodeFile struct {
	Rel     string // relative to the show's directory, e.g. "Season 1/x.S01E03.mkv"
	Name    string
	Season  int
	Episode int
}

// fileRef is a candidate episode file while the show is being read.
type fileRef struct {
	rel, name string
	dirSeason int
	special   int // 1-based number of a special (season 0), 0 for episodes
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
			files = append(files, fileRef{rel: e.Name, name: e.Name})
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
				files = append(files, fileRef{rel: path.Join(d.Name, k.Name), name: k.Name, dirSeason: seasonOfDirName(d.Name)})
			}
		}
	}
	// No season folders and no episodes of its own: the subfolders are copies or
	// seasons under other names (魔道祖师: two releases, 星蝶公主: two languages,
	// each holding its seasons in folders of its own, one of them wrapped once
	// more). A folder that names its season keeps it; of the others the one with
	// most episodes is the show, and the rest only fill the episodes it lacks
	// when their files name their own season.
	if seasonDirs == 0 && len(files) == 0 {
		type variant struct {
			refs   []fileRef
			season int
		}
		var named, unnamed []variant
		for i, d := range specialDirs {
			if i >= maxSeasonDirs {
				break
			}
			refs, err := variantRefs(list, d.Name, 0)
			if err != nil {
				return nil, err
			}
			v := variant{refs: refs, season: seasonOfDirName(d.Name)}
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
				if !ok || taken[[2]int{s, e}] {
					continue
				}
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
					files = append(files, fileRef{rel: path.Join(d.Name, vs[0].Name), name: vs[0].Name, special: specials})
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
		out = append(out, EpisodeFile{Rel: f.rel, Name: f.name, Season: s, Episode: e})
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
				refs = append(refs, fileRef{rel: path.Join(rel, k.Name), name: k.Name})
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
