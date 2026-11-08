package strm

import (
	"bytes"
	"context"
	"fmt"
	stdpath "path"
	"sort"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/media"
	"github.com/OpenListTeam/OpenList/v4/internal/media/nfo"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
)

// Media mode (Source = media): the tree is the media library, not a listing of
// a storage. Every work gets the folder and the file names the library's strm
// writer would give it (media.StrmTargets) and, next to each .strm, what a
// media server wants to read: the NFO, posters, fanart, episode thumbnails
// and the subtitles that sat next to the video. Listing and getting only look
// at the tree in memory; the storage under the library is touched only to
// read a subtitle file.

type mediaKind int

const (
	mediaStrm  mediaKind = iota + 1 // content made on demand by getLink
	mediaNFO                        // rendered when the tree is built
	mediaImage                      // read when first needed, see mediaImages
	mediaSub                        // the file of the library, read through its storage
)

// mediaNode is a folder or a file of the tree.
type mediaNode struct {
	name     string
	dir      bool
	kind     mediaKind
	modified time.Time
	raw      string // mediaStrm: the video's VFS path; mediaSub: the subtitle's VFS path
	data     []byte // mediaNFO
	size     int64  // mediaSub
	img      mediaImageSrc
	children map[string]*mediaNode
}

func newMediaDir(name string) *mediaNode {
	return &mediaNode{name: name, dir: true, children: map[string]*mediaNode{}}
}

// sub returns the node at the slash separated path below n, nil when there is none.
func (n *mediaNode) sub(p string) *mediaNode {
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		if !n.dir {
			return nil
		}
		if n = n.children[part]; n == nil {
			return nil
		}
	}
	return n
}

// add puts file at rel below n, creating the folders. It reports false and
// leaves the tree as it was when a node is already there.
func (n *mediaNode) add(rel string, file *mediaNode) bool {
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		next := n.children[part]
		if next == nil {
			next = newMediaDir(part)
			n.children[part] = next
		}
		if !next.dir {
			return false
		}
		n = next
	}
	file.name = parts[len(parts)-1]
	if _, ok := n.children[file.name]; ok {
		return false
	}
	n.children[file.name] = file
	return true
}

// mediaTree is the library as one tree per root of Paths, with the fingerprint
// of the library it was built from.
type mediaTree struct {
	roots map[string]*mediaNode
	stamp db.MediaLibraryStamp
}

func sameStamp(a, b db.MediaLibraryStamp) bool {
	return a.Items == b.Items && a.Updated.Equal(b.Updated) && a.Episodes == b.Episodes && a.Sidecars == b.Sidecars
}

// mediaTree returns the tree, rebuilt when the library changed: the count and
// latest update of the works (and the count of their episodes and sidecars)
// differ from those the tree was built from. The fingerprint is looked at once
// per d.recheck, so a long listing by a sync tool does not query for every
// directory. A failure keeps serving the tree it has; with none it is an error,
// never an empty library (a client that sees an empty folder deletes what it
// had).
func (d *Strm) mediaTree() (*mediaTree, error) {
	d.treeMu.RLock()
	t, fresh := d.tree, d.tree != nil && time.Since(d.treeChecked) < d.recheck
	d.treeMu.RUnlock()
	if fresh {
		return t, nil
	}
	d.treeMu.Lock()
	defer d.treeMu.Unlock()
	if d.tree != nil && time.Since(d.treeChecked) < d.recheck {
		return d.tree, nil
	}
	stamp, err := db.GetMediaLibraryStamp()
	if err != nil {
		if d.tree != nil {
			return d.tree, nil
		}
		return nil, err
	}
	d.treeChecked = time.Now()
	if d.tree != nil && sameStamp(d.tree.stamp, stamp) {
		return d.tree, nil
	}
	nt, err := d.buildMediaTree(stamp)
	if err != nil {
		if d.tree != nil {
			log.Warnf("[strm] rebuild media tree: %v", err)
			return d.tree, nil
		}
		return nil, err
	}
	d.tree = nt
	return nt, nil
}

// underAny reports whether p is one of the VFS paths or below one.
func underAny(p string, prefixes []string) bool {
	for _, pre := range prefixes {
		if pre == "/" || p == pre || strings.HasPrefix(p, strings.TrimSuffix(pre, "/")+"/") {
			return true
		}
	}
	return false
}

func (d *Strm) buildMediaTree(stamp db.MediaLibraryStamp) (*mediaTree, error) {
	items, err := db.ListMediaItemsForStrm()
	if err != nil {
		return nil, err
	}
	ids := make([]uint, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	eps, err := db.ListMediaEpisodesForItems(ids)
	if err != nil {
		return nil, err
	}
	sidecars, err := db.ListMediaSidecars(ids)
	if err != nil {
		return nil, err
	}
	t := &mediaTree{roots: map[string]*mediaNode{}, stamp: stamp}
	for key, prefixes := range d.pathMap {
		root := newMediaDir(key)
		for i := range items {
			it := &items[i]
			if underAny(db.MediaItemPath(it), prefixes) {
				addMediaItem(root, it, eps[it.ID], sidecars[it.ID])
			}
		}
		t.roots[key] = root
	}
	return t, nil
}

// relUnder is p relative to dir (both clean absolute VFS paths).
func relUnder(dir, p string) string {
	return strings.TrimPrefix(strings.TrimPrefix(p, strings.TrimSuffix(dir, "/")), "/")
}

// addMediaItem puts one work into root: its .strm files by media.StrmTargets
// and the files that go with them. A name that is already taken keeps its first
// owner (two works with the same title and year).
func addMediaItem(root *mediaNode, it *model.MediaItem, eps []model.MediaEpisode, sidecars []model.MediaSidecar) {
	targets := media.StrmTargetsWithEpisodes(it, eps)
	if len(targets) == 0 {
		return
	}
	video := it.MediaType == model.MediaTypeVideo
	scraped := video && media.IsScraped(it)
	episodic := video && model.IsEpisodicVideo(it.VideoType)
	// where the item's own relative paths (episode files, sidecars) start
	base := media.MediaItemFolder(it)
	if it.IsFolder {
		base = stdpath.Join(base, it.FileName)
	}
	epByRaw := map[string]*model.MediaEpisode{}
	if episodic && it.IsFolder {
		for i := range eps {
			ep := &eps[i]
			epByRaw[stdpath.Join(base, ep.FileName)] = ep
		}
	}
	subsOf := map[string][]model.MediaSidecar{}
	for _, s := range sidecars {
		subsOf[stdpath.Clean(s.Video)] = append(subsOf[stdpath.Clean(s.Video)], s)
	}

	var first string // RelPath of the first target that was taken
	seasons := map[string]int{}
	for _, tg := range targets {
		dir, strmName := stdpath.Split(tg.RelPath)
		dir = strings.TrimSuffix(dir, "/")
		stem := strings.TrimSuffix(strmName, ".strm")
		if !root.add(tg.RelPath, &mediaNode{kind: mediaStrm, raw: tg.RawPath, modified: it.UpdatedAt}) {
			log.Debugf("[strm] media: %s is taken, %s left out", tg.RelPath, tg.RawPath)
			continue
		}
		if first == "" {
			first = tg.RelPath
		}
		put := func(name string, n *mediaNode) {
			n.modified = it.UpdatedAt
			root.add(stdpath.Join(dir, name), n)
		}
		ep := epByRaw[tg.RawPath]
		if episodic {
			seasons[dir] = tg.Season
			if scraped {
				putNFO(put, stem+".nfo", media.EpisodeNFODetail(it, ep, tg.Season, tg.Episode))
			}
			if ep != nil && ep.Still != "" {
				put(stem+"-thumb.jpg", &mediaNode{kind: mediaImage, img: mediaImageSrc{itemID: it.ID, field: imageStill, episode: ep.FileName, stamp: it.UpdatedAt}})
			}
		} else if scraped {
			putNFO(put, stem+".nfo", withRoot(media.NFODetail(it), nfo.RootMovie))
		}
		// the subtitles of this video, renamed after the virtual one
		videoRel := relUnder(base, tg.RawPath)
		origStem := strings.TrimSuffix(stdpath.Base(videoRel), stdpath.Ext(videoRel))
		for _, s := range subsOf[videoRel] {
			rest := strings.TrimPrefix(stdpath.Base(s.Path), origStem)
			put(stem+rest, &mediaNode{kind: mediaSub, raw: stdpath.Join(base, s.Path), size: s.Size})
		}
	}
	if first == "" {
		return
	}

	// what belongs to the work as a whole sits in its folder: the film's, the show's, the album's
	top := stdpath.Dir(first)
	if episodic {
		top = strings.SplitN(first, "/", 2)[0]
		if scraped {
			putNFO(func(name string, n *mediaNode) { n.modified = it.UpdatedAt; root.add(stdpath.Join(top, name), n) }, "tvshow.nfo", withRoot(media.NFODetail(it), nfo.RootTVShow))
			seasonDirs := make([]string, 0, len(seasons))
			for dir := range seasons {
				seasonDirs = append(seasonDirs, dir)
			}
			sort.Strings(seasonDirs)
			for _, dir := range seasonDirs {
				s := seasons[dir]
				det := nfo.Detail{Root: nfo.RootSeason, Info: nfo.Info{Title: fmt.Sprintf("Season %d", s)}, Season: s}
				putNFO(func(name string, n *mediaNode) { n.modified = it.UpdatedAt; root.add(stdpath.Join(dir, name), n) }, "season.nfo", det)
			}
		}
	}
	image := func(name, field string) {
		root.add(stdpath.Join(top, name), &mediaNode{kind: mediaImage, modified: it.UpdatedAt, img: mediaImageSrc{itemID: it.ID, field: field, stamp: it.UpdatedAt}})
	}
	if it.Cover != "" {
		image("poster.jpg", imageCover)
	}
	if video && it.Backdrop != "" {
		image("fanart.jpg", imageBackdrop)
	}
}

func withRoot(d nfo.Detail, root string) nfo.Detail {
	d.Root = root
	return d
}

// putNFO renders d and, when that works, puts it with put.
func putNFO(put func(string, *mediaNode), name string, d nfo.Detail) {
	data, err := nfo.RenderDetail(d)
	if err != nil {
		log.Warnf("[strm] media: render %s: %v", name, err)
		return
	}
	put(name, &mediaNode{kind: mediaNFO, data: data})
}

// mediaLookup finds the node at path (a path of the storage) in the tree.
func (d *Strm) mediaLookup(path string) (*mediaNode, error) {
	t, err := d.mediaTree()
	if err != nil {
		return nil, err
	}
	root, sub := d.getRootAndPath(path)
	rn := t.roots[root]
	if rn == nil {
		return nil, errs.ObjectNotFound
	}
	n := rn.sub(sub)
	if n == nil {
		return nil, errs.ObjectNotFound
	}
	return n, nil
}

// mediaObj describes a node as an object at path. ok is false for an image that
// cannot be read: it is then not there as far as listings go, so a sync tool
// never fetches a file that would fail.
func (d *Strm) mediaObj(ctx context.Context, path string, n *mediaNode) (model.Obj, bool) {
	obj := &model.Object{Path: path, Name: n.name, IsFolder: n.dir, Modified: n.modified}
	switch {
	case n.dir:
		obj.Modified = d.Modified
	case n.kind == mediaStrm:
		obj.Size = int64(len(d.getLink(ctx, n.raw)))
	case n.kind == mediaNFO:
		obj.Size = int64(len(n.data))
	case n.kind == mediaSub:
		obj.Size = n.size
	case n.kind == mediaImage:
		size, ok := d.images.size(ctx, n.img)
		if !ok {
			return nil, false
		}
		obj.Size = size
	}
	return obj, true
}

func (d *Strm) getMedia(ctx context.Context, path string) (model.Obj, error) {
	if utils.PathEqual(path, "/") {
		return &model.Object{Path: "/", Name: "/", IsFolder: true, Modified: d.Modified}, nil
	}
	n, err := d.mediaLookup(path)
	if err != nil {
		return nil, err
	}
	obj, ok := d.mediaObj(ctx, path, n)
	if !ok {
		return nil, errs.ObjectNotFound
	}
	return obj, nil
}

func (d *Strm) listMedia(ctx context.Context, path string) ([]model.Obj, error) {
	n, err := d.mediaLookup(path)
	if err != nil {
		return nil, err
	}
	if !n.dir {
		return nil, errs.NotFolder
	}
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	// the images of a folder are looked at together: their sizes are what is listed
	var imgs []mediaImageSrc
	for _, name := range names {
		if c := n.children[name]; c.kind == mediaImage {
			imgs = append(imgs, c.img)
		}
	}
	d.images.warm(ctx, imgs)
	objs := make([]model.Obj, 0, len(names))
	for _, name := range names {
		if obj, ok := d.mediaObj(ctx, stdpath.Join(path, name), n.children[name]); ok {
			objs = append(objs, obj)
		}
	}
	return objs, nil
}

func (d *Strm) linkMedia(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	n, err := d.mediaLookup(file.GetPath())
	if err != nil {
		return nil, err
	}
	var data []byte
	switch n.kind {
	case mediaStrm:
		data = []byte(d.getLink(ctx, n.raw))
	case mediaNFO:
		data = n.data
	case mediaImage:
		if data, err = d.images.get(ctx, n.img); err != nil {
			return nil, err
		}
	case mediaSub:
		return d.linkFile(ctx, n.raw, args)
	default:
		return nil, errs.NotFile
	}
	return &model.Link{RangeReader: stream.GetRangeReaderFromMFile(int64(len(data)), bytes.NewReader(data))}, nil
}
