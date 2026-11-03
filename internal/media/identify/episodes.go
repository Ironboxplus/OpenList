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
	type fileRef struct {
		rel, name string
		dirSeason int
		special   int // 1-based number of a special (season 0), 0 for episodes
	}
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
	// seasons under other names (魔道祖师: two releases, 星蝶公主: two languages).
	// A folder that names its season keeps it; of the others the one with most
	// episodes is the show and the rest are taken for copies of it.
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
			kids, err := list(d.Name)
			if err != nil {
				return nil, fmt.Errorf("list %s: %w", d.Name, err)
			}
			v := variant{season: seasonOfDirName(d.Name)}
			for _, k := range kids {
				if _, _, ok := EpisodeOf(k.Name); ok && !k.IsDir && IsVideo(k.Name) {
					v.refs = append(v.refs, fileRef{rel: path.Join(d.Name, k.Name), name: k.Name, dirSeason: v.season})
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
		taken := map[int]bool{}
		for _, v := range named {
			taken[v.season] = true
			files = append(files, v.refs...)
		}
		if len(unnamed) > 0 {
			best := unnamed[0]
			for _, v := range unnamed[1:] {
				if len(v.refs) > len(best.refs) {
					best = v
				}
			}
			s := 1
			for taken[s] {
				s++
			}
			for _, r := range best.refs {
				r.dirSeason = s
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
