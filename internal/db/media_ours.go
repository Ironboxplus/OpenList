// Staged copy of our media DB layer (PLAN_MERGE_UPSTREAM_BE 2.3); removed in step 4.11.

package db

import (
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// mediaListColumns is the whitelist of columns the list endpoints read. The
// heavy ones (overview, candidates) are never selected for lists.
var mediaListColumns = []string{"id", "path", "kind", "title", "original_title", "year", "ratings", "poster_id", "source", "confidence", "locked", "candidates"}

// mediaChildColumns is what a directory view needs for each child.
var mediaChildColumns = []string{"id", "path", "kind", "title", "year", "ratings", "poster_id"}

// likeEscape escapes LIKE wildcards; the escape character is '!' because it
// means the same in every SQL dialect we support (a backslash does not).
func likeEscape(s string) string {
	s = strings.ReplaceAll(s, "!", "!!")
	s = strings.ReplaceAll(s, "%", "!%")
	return strings.ReplaceAll(s, "_", "!_")
}

// GetMediaWorkByPath returns (nil, nil) when there is no item at path.
func GetMediaWorkByPath(path string) (*model.MediaWork, error) {
	// Find+Limit rather than First: a missing item is normal and must not
	// log "record not found".
	var items []model.MediaWork
	if err := db.Where("path = ?", path).Limit(1).Find(&items).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	return &items[0], nil
}

// GetMediaWorksByPaths returns the items at exactly these paths.
func GetMediaWorksByPaths(paths []string) ([]model.MediaWork, error) {
	var items []model.MediaWork
	if len(paths) == 0 {
		return items, nil
	}
	if err := db.Where("path IN ?", paths).Find(&items).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return items, nil
}

// ListMediaWorkChildren returns the items that sit directly under parent, with the
// list-column whitelist only.
func ListMediaWorkChildren(parent string) ([]model.MediaWork, error) {
	prefix := strings.TrimRight(parent, "/") + "/"
	var items []model.MediaWork
	err := db.Select(mediaChildColumns).
		Where("path LIKE ? ESCAPE '!'", likeEscape(prefix)+"%").
		Where("path NOT LIKE ? ESCAPE '!'", likeEscape(prefix)+"%/%").
		Order("path").Find(&items).Error
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return items, nil
}

// MediaWorkListFilter selects items for the admin list.
type MediaWorkListFilter struct {
	Prefix  string // path or directory prefix; "" or "/" = everything
	Kind    string
	Kinds   []string // any of these; combined with Kind by AND
	Pending bool
	Page    int
	PerPage int
}

func (f MediaWorkListFilter) apply(q *gorm.DB) *gorm.DB {
	if p := strings.TrimRight(f.Prefix, "/"); p != "" {
		q = q.Where("(path = ? OR path LIKE ? ESCAPE '!')", p, likeEscape(p)+"/%")
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if len(f.Kinds) > 0 {
		q = q.Where("kind IN ?", f.Kinds)
	}
	if f.Pending {
		// not locked, and either low confidence or candidates waiting for a choice
		q = q.Where("locked = ?", false).
			Where("(confidence < ? OR (candidates <> '' AND candidates <> '[]' AND candidates <> 'null'))", model.MediaPendingConfidence)
	}
	return q
}

func ListMediaWorks(f MediaWorkListFilter) ([]model.MediaWork, int64, error) {
	var total int64
	if err := f.apply(db.Model(&model.MediaWork{})).Count(&total).Error; err != nil {
		return nil, 0, errors.WithStack(err)
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PerPage < 1 || f.PerPage > 1000 {
		f.PerPage = 100
	}
	var items []model.MediaWork
	err := f.apply(db.Select(mediaListColumns)).Order("path").
		Offset((f.Page - 1) * f.PerPage).Limit(f.PerPage).Find(&items).Error
	if err != nil {
		return nil, 0, errors.WithStack(err)
	}
	return items, total, nil
}

// mediaFilterBatch is a variable so a test can cross batch boundaries cheaply.
var mediaFilterBatch = 500

// ListMediaWorksFiltered pages through the items keep accepts: it reads them
// in path order a batch at a time, drops what keep rejects and only then takes
// the page, so a page is never short of items a caller may see and total is
// the filtered count. Page and PerPage are normalised like ListMediaWorks.
func ListMediaWorksFiltered(f MediaWorkListFilter, keep func(*model.MediaWork) bool) ([]model.MediaWork, int64, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PerPage < 1 || f.PerPage > 1000 {
		f.PerPage = 100
	}
	from := int64(f.Page-1) * int64(f.PerPage)
	to := from + int64(f.PerPage)
	var (
		out   []model.MediaWork
		total int64
		last  string
	)
	for {
		var batch []model.MediaWork
		q := f.apply(db.Select(mediaChildColumns))
		if last != "" {
			q = q.Where("path > ?", last)
		}
		if err := q.Order("path").Limit(mediaFilterBatch).Find(&batch).Error; err != nil {
			return nil, 0, errors.WithStack(err)
		}
		for i := range batch {
			if keep != nil && !keep(&batch[i]) {
				continue
			}
			if total >= from && total < to {
				out = append(out, batch[i])
			}
			total++
		}
		if len(batch) < mediaFilterBatch {
			return out, total, nil
		}
		last = batch[len(batch)-1].Path
	}
}

// AllMediaWorks is for export.
func AllMediaWorks(prefix string) ([]model.MediaWork, error) {
	var items []model.MediaWork
	if err := (MediaWorkListFilter{Prefix: prefix}).apply(db.Model(&model.MediaWork{})).Order("path").Find(&items).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return items, nil
}

// SaveMediaWork inserts or updates (by primary key) the item, and when
// episodes is non-nil replaces its episodes, in one transaction.
func SaveMediaWork(it *model.MediaWork, episodes []model.MediaWorkEpisode) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(it).Error; err != nil {
			return err
		}
		if episodes == nil {
			return nil
		}
		if err := tx.Where("item_id = ?", it.ID).Delete(&model.MediaWorkEpisode{}).Error; err != nil {
			return err
		}
		for i := range episodes {
			episodes[i].ID = 0
			episodes[i].ItemID = it.ID
		}
		if len(episodes) == 0 {
			return nil
		}
		return tx.CreateInBatches(episodes, 200).Error
	}))
}

func GetMediaWorkEpisodes(itemID uint) ([]model.MediaWorkEpisode, error) {
	var eps []model.MediaWorkEpisode
	if err := db.Where("item_id = ?", itemID).Order("season, episode, id").Find(&eps).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return eps, nil
}

// DeleteMediaWorkByPath removes one item and its episodes; reports how many
// items went.
func DeleteMediaWorkByPath(path string) (int64, error) {
	return deleteMediaWorks(db.Where("path = ?", path))
}

// DeleteMediaWorksByPrefix removes the item at prefix and everything below it.
func DeleteMediaWorksByPrefix(prefix string) (int64, error) {
	p := strings.TrimRight(prefix, "/")
	if p == "" {
		return deleteMediaWorks(db.Where("1 = 1"))
	}
	return deleteMediaWorks(db.Where("(path = ? OR path LIKE ? ESCAPE '!')", p, likeEscape(p)+"/%"))
}

func deleteMediaWorks(cond *gorm.DB) (int64, error) {
	var n int64
	err := db.Transaction(func(tx *gorm.DB) error {
		var ids []uint
		if err := tx.Model(&model.MediaWork{}).Where(cond).Pluck("id", &ids).Error; err != nil {
			return err
		}
		n = int64(len(ids))
		for i := 0; i < len(ids); i += 500 {
			end := min(i+500, len(ids))
			chunk := ids[i:end]
			if err := tx.Where("item_id IN ?", chunk).Delete(&model.MediaWorkEpisode{}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", chunk).Delete(&model.MediaWork{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return n, errors.WithStack(err)
}

// ---- assets ----

func GetMediaWorkAsset(id uint) (*model.MediaWorkAsset, error) {
	var as []model.MediaWorkAsset
	if err := db.Where("id = ?", id).Limit(1).Find(&as).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	if len(as) == 0 {
		return nil, nil
	}
	return &as[0], nil
}

func GetMediaWorkAssetBySHA(sha string) (*model.MediaWorkAsset, error) {
	var as []model.MediaWorkAsset
	if err := db.Where("sha256 = ?", sha).Limit(1).Find(&as).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	if len(as) == 0 {
		return nil, nil
	}
	return &as[0], nil
}

func GetMediaWorkAssets(ids []uint) ([]model.MediaWorkAsset, error) {
	var as []model.MediaWorkAsset
	if len(ids) == 0 {
		return as, nil
	}
	if err := db.Where("id IN ?", ids).Find(&as).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return as, nil
}

func CreateMediaWorkAsset(a *model.MediaWorkAsset) error {
	return errors.WithStack(db.Create(a).Error)
}

// ListMediaWorkPostersUnder returns, in one query, every item below parent that
// has a poster (path, title, year, poster_id only). The caller groups them per
// collection.
func ListMediaWorkPostersUnder(parent string) ([]model.MediaWork, error) {
	prefix := strings.TrimRight(parent, "/") + "/"
	var items []model.MediaWork
	err := db.Select("id", "path", "title", "year", "poster_id").
		Where("poster_id > 0").
		Where("path LIKE ? ESCAPE '!'", likeEscape(prefix)+"%").
		Find(&items).Error
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return items, nil
}
