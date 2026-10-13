package handles

import (
	"fmt"
	stdpath "path"
	"sort"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// Sidecar subtitle sources reported in driver.VideoSubtitleInfo.Source.
const (
	subtitleSourceSidecar  = "sidecar"
	subtitleSourceProvider = "provider"
)

// subtitleExts are the sidecar subtitle file types matched as same-name siblings
// of a video. Matching is case-insensitive.
var subtitleExts = map[string]bool{
	"srt": true, "vtt": true, "ass": true, "ssa": true, "sup": true,
}

// subtitleDirNames are the folder names (lowercase) that conventionally hold
// subtitles next to a video.
var subtitleDirNames = map[string]bool{
	"subs": true, "sub": true, "subtitles": true, "字幕": true,
}

// dirListing is the names found in one folder.
type dirListing struct {
	Files []string
	Dirs  []string
}

// sidecarMatch is one subtitle file claimed by a video. Dir is relative to the
// video's folder ("" = same folder, "Subs", "Subs/<video stem>").
type sidecarMatch struct {
	Dir  string
	Name string
	Ext  string
}

// matchSidecarSubtitle reports whether sibling is a same-name sidecar subtitle of
// the video (basename.<...>.<ext> with ext ∈ subtitleExts), returning the
// lowercased subtitle extension. It requires a '.' right after the video's
// basename so "Movie2.srt" doesn't match video "Movie.mkv", and ignores the file
// that is the video itself.
func matchSidecarSubtitle(videoName, siblingName string) (string, bool) {
	if strings.EqualFold(videoName, siblingName) {
		return "", false
	}
	ext := subtitleExt(siblingName)
	if ext == "" {
		return "", false
	}
	base := strings.ToLower(strings.TrimSuffix(videoName, stdpath.Ext(videoName)))
	sib := strings.ToLower(siblingName)
	if !strings.HasPrefix(sib, base) || !strings.HasPrefix(sib[len(base):], ".") {
		return "", false
	}
	return ext, true
}

// subtitleExt returns the lowercased subtitle extension of name, or "" if name
// is not a subtitle file.
func subtitleExt(name string) string {
	ext := strings.TrimPrefix(strings.ToLower(stdpath.Ext(name)), ".")
	if !subtitleExts[ext] {
		return ""
	}
	return ext
}

// subtitleDirKeys returns the listed top-level subtitle folders (Subs, 字幕, …),
// sorted, from a listing whose "" entry is the video's own folder.
func subtitleDirKeys(listing map[string]dirListing) []string {
	var keys []string
	for _, d := range listing[""].Dirs {
		if _, ok := listing[d]; ok && subtitleDirNames[strings.ToLower(d)] {
			keys = append(keys, d)
		}
	}
	sort.Strings(keys)
	return keys
}

// pendingSubtitleDirs returns the folders that must still be listed to resolve
// the video's subtitles given what has been listed so far: the subtitle folders
// beside the video, then (RARBG style) a folder named after the video inside
// them. Folders are only ever requested when they exist, so videos without
// subtitle folders cost no extra listing. Call until it returns nothing.
func pendingSubtitleDirs(videoName string, listing map[string]dirListing) []string {
	var want []string
	for _, d := range listing[""].Dirs {
		if _, done := listing[d]; !done && subtitleDirNames[strings.ToLower(d)] {
			want = append(want, d)
		}
	}
	stem := videoStem(videoName)
	for _, key := range subtitleDirKeys(listing) {
		for _, d := range listing[key].Dirs {
			child := key + "/" + d
			if _, done := listing[child]; !done && strings.EqualFold(d, stem) {
				want = append(want, child)
			}
		}
	}
	sort.Strings(want)
	return want
}

func videoStem(videoName string) string {
	return strings.TrimSuffix(videoName, stdpath.Ext(videoName))
}

// collectSidecarSubtitles returns the subtitle files that belong to videoName,
// in priority order: (a) same-name prefix in the video's folder, (b) inside a
// subtitle folder — same-name prefix, or any subtitle in a folder named after the
// video (RARBG: Subs/<video stem>/2_English.srt), (c) any remaining subtitle when
// the video is the only video in its folder and subtitle folders. Within a
// category results are sorted by path.
//
// Not supported on purpose: subtitles whose name differs from the video when the
// folder holds several videos (e.g. an episode list whose subtitle naming does
// not follow the video naming). Which subtitle goes with which episode would be a
// guess, and a wrong subtitle is worse than none.
func collectSidecarSubtitles(videoName string, listing map[string]dirListing) []sidecarMatch {
	claimed := map[string]bool{}
	var a, b, c []sidecarMatch
	add := func(dst *[]sidecarMatch, dir, name, ext string) {
		key := dir + "/" + name
		if claimed[key] {
			return
		}
		claimed[key] = true
		*dst = append(*dst, sidecarMatch{Dir: dir, Name: name, Ext: ext})
	}

	for _, name := range listing[""].Files {
		if ext, ok := matchSidecarSubtitle(videoName, name); ok {
			add(&a, "", name, ext)
		}
	}

	subDirs := subtitleDirKeys(listing)
	stem := videoStem(videoName)
	for _, key := range subDirs {
		for _, name := range listing[key].Files {
			if ext, ok := matchSidecarSubtitle(videoName, name); ok {
				add(&b, key, name, ext)
			}
		}
		for _, d := range listing[key].Dirs {
			if !strings.EqualFold(d, stem) {
				continue
			}
			child := key + "/" + d
			for _, name := range listing[child].Files {
				if ext := subtitleExt(name); ext != "" {
					add(&b, child, name, ext)
				}
			}
		}
	}

	videos := 0
	for _, key := range append([]string{""}, subDirs...) {
		for _, name := range listing[key].Files {
			if utils.GetFileType(name) == conf.VIDEO {
				videos++
			}
		}
	}
	if videos == 1 && utils.GetFileType(videoName) == conf.VIDEO {
		for _, key := range append([]string{""}, subDirs...) {
			for _, name := range listing[key].Files {
				if strings.EqualFold(name, videoName) {
					continue
				}
				if ext := subtitleExt(name); ext != "" {
					add(&c, key, name, ext)
				}
			}
		}
	}

	var out []sidecarMatch
	for _, group := range [][]sidecarMatch{a, b, c} {
		sort.SliceStable(group, func(i, j int) bool {
			return group[i].Dir+"/"+group[i].Name < group[j].Dir+"/"+group[j].Name
		})
		out = append(out, group...)
	}
	return out
}

// resolveSidecarSubtitles lists the video's folder (and, only when present, its
// subtitle folders) and returns the matching subtitle files as VideoSubtitleInfo
// with signed same-origin /p proxy URLs (mirroring the frontend proxyLink + FsGet
// rawURL construction).
func resolveSidecarSubtitles(c *gin.Context, reqPath string, video model.Obj) []driver.VideoSubtitleInfo {
	parentPath := stdpath.Dir(reqPath)
	listing := map[string]dirListing{}
	objs := map[string]model.Obj{} // key: dir + "/" + name
	listDir := func(key string) bool {
		list, err := fs.List(c.Request.Context(), stdpath.Join(parentPath, key), &fs.ListArgs{})
		if err != nil {
			return false
		}
		var dl dirListing
		for _, o := range list {
			if o.IsDir() {
				dl.Dirs = append(dl.Dirs, o.GetName())
				continue
			}
			dl.Files = append(dl.Files, o.GetName())
			objs[key+"/"+o.GetName()] = o
		}
		listing[key] = dl
		return true
	}
	if !listDir("") {
		return nil
	}
	for {
		pending := pendingSubtitleDirs(video.GetName(), listing)
		progressed := false
		for _, key := range pending {
			if !listDir(key) {
				listing[key] = dirListing{} // do not ask again
			}
			progressed = true
		}
		if !progressed {
			break
		}
	}

	apiURL := common.GetApiUrl(c)
	var subs []driver.VideoSubtitleInfo
	for _, m := range collectSidecarSubtitles(video.GetName(), listing) {
		dirPath := stdpath.Join(parentPath, m.Dir)
		dirMeta, _ := op.GetNearestMeta(dirPath)
		query := ""
		if s := common.Sign(objs[m.Dir+"/"+m.Name], dirPath, isEncrypt(dirMeta, dirPath)); s != "" {
			query = "?sign=" + s
		}
		subs = append(subs, driver.VideoSubtitleInfo{
			Title:  strings.TrimSuffix(m.Name, stdpath.Ext(m.Name)),
			Type:   m.Ext,
			URL:    fmt.Sprintf("%s/p%s%s", apiURL, utils.EncodePath(stdpath.Join(dirPath, m.Name), true), query),
			Source: subtitleSourceSidecar,
		})
	}
	return subs
}
