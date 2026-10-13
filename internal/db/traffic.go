package db

import (
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AddTrafficDaily adds delta bytes to the (day, kind) counter, creating it.
func AddTrafficDaily(day, kind string, delta int64) error {
	row := model.TrafficDaily{Day: day, Kind: kind, Bytes: delta}
	err := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "day"}, {Name: "kind"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"bytes": gorm.Expr("traffic_dailies.bytes + ?", delta),
		}),
	}).Create(&row).Error
	return errors.WithStack(err)
}

// TrafficDailySince returns the counters of every day >= fromDay.
func TrafficDailySince(fromDay string) ([]model.TrafficDaily, error) {
	var rows []model.TrafficDaily
	err := db.Where("day >= ?", fromDay).Order("day").Find(&rows).Error
	return rows, errors.WithStack(err)
}

// TrafficSums sums the counters per kind over days in [fromDay, toDay]. Empty
// bounds are open.
func TrafficSums(fromDay, toDay string) (map[string]int64, error) {
	q := db.Model(&model.TrafficDaily{}).Select("kind, SUM(bytes) AS bytes").Group("kind")
	if fromDay != "" {
		q = q.Where("day >= ?", fromDay)
	}
	if toDay != "" {
		q = q.Where("day <= ?", toDay)
	}
	var rows []model.TrafficDaily
	if err := q.Scan(&rows).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Kind] = r.Bytes
	}
	return out, nil
}

// MediaStats counts the scraped library the way /api/media/library lists it:
// the work kinds only ("ignore" and "library" roots are not works).
type MediaStats struct {
	Movie      int64 `json:"movie"`
	TV         int64 `json:"tv"`
	Anime      int64 `json:"anime"`
	Collection int64 `json:"collection"`
	Episodes   int64 `json:"episodes"`
	// Unscraped works nothing has been fetched for yet.
	Unscraped int64 `json:"unscraped"`
}

func GetMediaStats() (MediaStats, error) {
	var out MediaStats
	type kc struct {
		Kind string
		N    int64
	}
	var rows []kc
	err := db.Model(&model.MediaItem{}).Select("kind, COUNT(*) AS n").
		Where("kind IN ?", []string{model.MediaKindMovie, model.MediaKindTV, model.MediaKindAnime, model.MediaKindCollection}).
		Group("kind").Scan(&rows).Error
	if err != nil {
		return out, errors.WithStack(err)
	}
	for _, r := range rows {
		switch r.Kind {
		case model.MediaKindMovie:
			out.Movie = r.N
		case model.MediaKindTV:
			out.TV = r.N
		case model.MediaKindAnime:
			out.Anime = r.N
		case model.MediaKindCollection:
			out.Collection = r.N
		}
	}
	if err := db.Model(&model.MediaEpisode{}).Count(&out.Episodes).Error; err != nil {
		return out, errors.WithStack(err)
	}
	err = db.Model(&model.MediaItem{}).
		Where("kind NOT IN ?", []string{model.MediaKindIgnore, model.MediaKindLibrary}).
		Where("scraped_at IS NULL").Count(&out.Unscraped).Error
	return out, errors.WithStack(err)
}
