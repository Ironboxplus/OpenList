package syncjob

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// packUploads sends finished packs to dst while the builder goes on with the
// next one. Each upload is one connection, so several at once is what lets a
// job fill a link that spreads connections over lines; at most limit are in
// flight, which is also what bounds the staging area (limit packs uploading
// plus the one being built).
//
// A failed pack is not re-sent here: rclone retries inside the copy, and a run
// that still fails is retried as a whole.
//
// Uploads come in batches: wait ends the batch, reports its first failure, and
// the next start begins a new one. start and wait are meant for the one
// goroutine that builds packs.
type packUploads struct {
	ctx   context.Context
	limit int

	g    *errgroup.Group
	gctx context.Context // done once an upload of the batch fails
}

func newPackUploads(ctx context.Context, limit int) *packUploads {
	u := &packUploads{ctx: ctx, limit: max(limit, 1)}
	u.newBatch()
	return u
}

func (u *packUploads) newBatch() {
	u.g, u.gctx = errgroup.WithContext(u.ctx)
	u.g.SetLimit(u.limit)
}

// start runs upload in the background, waiting for a free slot first. ctx is
// the builder's context; the upload also stops when another one has failed.
// It returns the failure of an earlier upload, so the builder doesn't go on
// making packs nobody will commit.
func (u *packUploads) start(ctx context.Context, upload func(context.Context) error) error {
	if err := u.failure(); err != nil {
		return err
	}
	u.g.Go(func() error {
		uctx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		defer context.AfterFunc(u.gctx, func() { cancel(context.Cause(u.gctx)) })()
		return upload(uctx)
	})
	return u.failure()
}

// wait blocks until every upload of the batch is done and returns the first
// failure.
func (u *packUploads) wait() error {
	err := u.g.Wait()
	u.newBatch() // Wait cancels the group's context
	return err
}

func (u *packUploads) failure() error {
	if u.gctx.Err() == nil {
		return nil
	}
	return context.Cause(u.gctx)
}
