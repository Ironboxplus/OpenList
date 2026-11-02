package model

// MediaRating is one source's rating of a work. MediaItem.Rating keeps the main
// rating for the places that show only one number; this table holds all of
// them (tmdb, imdb, douban, bgm, and Rotten Tomatoes as text).
type MediaRating struct {
	ID     uint    `gorm:"primarykey"                                    json:"-"`
	ItemID uint    `gorm:"not null;uniqueIndex:idx_media_rating_source"  json:"item_id"`
	Source string  `gorm:"size:32;not null;uniqueIndex:idx_media_rating_source" json:"source"`
	Value  float32 `json:"value"`                                 // 0-10; 0 when only Text is known
	Text   string  `gorm:"size:32"         json:"text,omitempty"` // as the source writes it, e.g. "93%"
}

// Rating sources, in the order that picks the main rating.
var MediaRatingSources = []string{"tmdb", "douban", "imdb", "bgm", "rt"}

// PrimaryRating picks the number MediaItem.Rating shows from a set of ratings:
// the first source in MediaRatingSources that has a numeric value.
func PrimaryRating(ratings map[string]float32) float32 {
	for _, s := range MediaRatingSources {
		if v := ratings[s]; v > 0 {
			return v
		}
	}
	return 0
}
