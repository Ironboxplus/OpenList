package identify

import "path"

// maxUnwrap is how many wrapper directories one listing opens.
const maxUnwrap = 64

// UnwrapEntries replaces each directory that is named like a video file and
// holds exactly one video by that video, with the directory as Via. 115's
// offline downloads leave every file in a directory of its own name
// ("[ANi] X - 03 [1080P].mp4/[ANi] X - 03 [1080P].mp4"), which would otherwise
// make the directory of a show look like a set of films. peek lists one of the
// directories; without it, or when it fails or finds anything but one video,
// the entry stays as it was. At most maxUnwrap directories are opened.
func UnwrapEntries(entries []Entry, peek Peek) []Entry {
	if peek == nil || len(entries) == 0 {
		return entries
	}
	out := make([]Entry, 0, len(entries))
	opened := 0
	for _, e := range entries {
		if !e.IsDir || !IsVideo(e.Name) || opened >= maxUnwrap {
			out = append(out, e)
			continue
		}
		opened++
		kids, err := peek(e.Name)
		if err != nil {
			out = append(out, e)
			continue
		}
		var videos []Entry
		for _, k := range kids {
			if !k.IsDir && IsVideo(k.Name) {
				videos = append(videos, k)
			}
		}
		if videos = mainVideos(videos); len(videos) != 1 {
			out = append(out, e)
			continue
		}
		out = append(out, Entry{Name: videos[0].Name, Size: videos[0].Size, Via: e.Name})
	}
	return out
}

// Rel is where the entry lives relative to the directory that was listed: its
// name, behind the wrapper directory it was unwrapped from.
func (e Entry) Rel() string {
	if e.Via != "" {
		return path.Join(e.Via, e.Name)
	}
	return e.Name
}

// ItemName is the name of the library item the entry is: the wrapper
// directory when it has one, else the entry itself.
func (e Entry) ItemName() string {
	if e.Via != "" {
		return e.Via
	}
	return e.Name
}

// holdsOneVideo reports whether a listing is one video and nothing else that
// could be a work (extras and loose files do not count).
func holdsOneVideo(entries []Entry) bool {
	var videos []Entry
	for _, e := range entries {
		switch {
		case e.IsDir:
			if !isExtraDirName(e.Name) {
				return false
			}
		case IsVideo(e.Name):
			videos = append(videos, e)
		}
	}
	return len(mainVideos(videos)) == 1
}
