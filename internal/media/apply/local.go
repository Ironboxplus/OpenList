package apply

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/media"
	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/store"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	pkgerr "github.com/pkg/errors"
)

// LocalLister lists through fs.List inside the server. It passes no Refresh,
// so cloud drives are asked only when their cache has nothing. The ctx must
// carry the acting user (conf.UserKey) for the hide rules.
type LocalLister struct{}

func (LocalLister) List(ctx context.Context, p string) ([]identify.Entry, error) {
	objs, err := fs.List(ctx, p, &fs.ListArgs{NoLog: true})
	if err != nil {
		if errs.IsObjectNotFound(err) || pkgerr.Cause(err) == errs.NotFolder {
			return nil, ErrNotDir
		}
		return nil, err
	}
	out := make([]identify.Entry, 0, len(objs))
	for _, o := range objs {
		out = append(out, identify.Entry{Name: o.GetName(), IsDir: o.IsDir(), Size: o.GetSize()})
	}
	return out, nil
}

// ReadFile reads at most max bytes from the start of the file at p.
func (LocalLister) ReadFile(ctx context.Context, p string, max int64) ([]byte, error) {
	rc := media.FetchFileReader(ctx, p)
	if rc == nil {
		return nil, errors.New("cannot open " + p)
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, max))
}

// LocalWriter writes through the store inside the server.
type LocalWriter struct{ Store *store.Store }

func (w LocalWriter) Existing(_ context.Context, prefix string) (map[string]Existing, error) {
	items, err := db.ListVideoItemsUnder(prefix)
	if err != nil {
		return nil, err
	}
	m := make(map[string]Existing, len(items))
	for i := range items {
		it := &items[i]
		p := db.MediaItemPath(it)
		e := Existing{Path: p, Kind: it.VideoType, Title: it.ScrapedName, Locked: it.Locked, Confidence: it.Confidence, HasBackdrop: it.Backdrop != ""}
		if it.ExternalIDs != "" {
			_ = json.Unmarshal([]byte(it.ExternalIDs), &e.ExternalIDs)
		}
		m[p] = e
	}
	return m, nil
}

func (w LocalWriter) SaveImage(ctx context.Context, p, role string, data []byte) (string, error) {
	return w.Store.SaveImage(ctx, p, role, data)
}

func (w LocalWriter) Set(_ context.Context, req model.MediaSetReq, force bool) error {
	req.Path = path.Clean("/" + req.Path)
	_, err := w.Store.Set(req, store.SetOptions{RespectLock: !force})
	if err == store.ErrLocked {
		return ErrLocked
	}
	return err
}
