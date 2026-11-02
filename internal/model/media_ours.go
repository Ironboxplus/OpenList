// Staged copy of our media model, kept next to the upstream MediaItem model until
// it is folded into it (PLAN_MERGE_UPSTREAM_BE section 4). The renamed types map to
// the tables media_works, media_work_episodes and media_work_assets by gorm's default
// naming (table prefix included). Delete this file in step 4.11.

package model

import "time"

// Kinds that exist only for the staged works: a library root.
const MediaKindLibrary = "library"

// MediaWork is a scraped work. P1 models "directory = one work"; the group
// tables of REFACTOR_PLAN 6.6 can be derived from it later.
type MediaWork struct {
	ID            uint       `json:"id" gorm:"primaryKey"`
	Path          string     `json:"path" gorm:"uniqueIndex;size:700"` // OP virtual path, FixAndCleanPath'd
	Kind          string     `json:"kind"`                             // movie | tv | anime | collection | ignore
	Title         string     `json:"title"`
	OriginalTitle string     `json:"original_title"`
	Year          int        `json:"year"`
	ExternalIDs   string     `json:"external_ids"` // JSON {"tmdb":"tv/890","imdb":"tt0112159","bgm":"265","douban":"1291843"}
	Ratings       string     `json:"ratings"`      // JSON {"tmdb":8.3,"imdb":8.5,"douban":9.3,"bgm":8.9,"rt":"..."}
	Genres        string     `json:"genres"`       // JSON ["动画","科幻"]
	Overview      string     `json:"overview"`
	PosterID      uint       `json:"poster_id"` // media_assets.id, 0 = none
	BackdropID    uint       `json:"backdrop_id"`
	Source        string     `json:"source"` // manual | rule | scraper
	Confidence    float32    `json:"confidence"`
	Locked        bool       `json:"locked"`     // true: automatic scans never overwrite
	Candidates    string     `json:"candidates"` // JSON: top candidates when confidence is low
	ScrapedAt     *time.Time `json:"scraped_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

// MediaWorkEpisode maps a file inside a tv/anime item to an episode.
type MediaWorkEpisode struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	ItemID   uint   `json:"item_id" gorm:"index"`
	FileName string `json:"file_name"` // relative to the item path, e.g. "Season 1/xxx.S01E03.mkv"
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Title    string `json:"title"`
	Overview string `json:"overview"`
	StillID  uint   `json:"still_id"`
}

// MediaWorkAsset is an image stored on disk under <data>/media/assets, never in
// the database.
type MediaWorkAsset struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	SHA256    string    `json:"sha256" gorm:"uniqueIndex;size:64"`
	Mime      string    `json:"mime"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	SourceURL string    `json:"source_url"` // where it came from; informational and used by import
	CreatedAt time.Time `json:"created_at"`
}
