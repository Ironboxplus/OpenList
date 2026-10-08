package model

import "time"

// Media kinds. A MediaItem is one directory (or, for a loose video at the top
// level, one file) and its path is its identity.
const (
	MediaKindMovie      = "movie"
	MediaKindTV         = "tv"
	MediaKindAnime      = "anime"
	MediaKindCollection = "collection"
	MediaKindIgnore     = "ignore"
	MediaKindLibrary    = "library" // a root whose entries are the works: opens as a poster wall
)

// Where an item's data came from.
const (
	MediaSourceManual  = "manual"
	MediaSourceRule    = "rule"
	MediaSourceScraper = "scraper"
)

// MediaPendingConfidence is the confidence below which an unlocked item still
// needs a human look.
const MediaPendingConfidence = 0.6

func ValidMediaKind(k string) bool {
	switch k {
	case MediaKindMovie, MediaKindTV, MediaKindAnime, MediaKindCollection, MediaKindIgnore, MediaKindLibrary:
		return true
	}
	return false
}

// MediaItem is a scraped work. P1 models "directory = one work"; the group
// tables of REFACTOR_PLAN 6.6 can be derived from it later.
type MediaItem struct {
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

// MediaEpisode maps a file inside a tv/anime item to an episode.
type MediaEpisode struct {
	ID       uint   `json:"id" gorm:"primaryKey"`
	ItemID   uint   `json:"item_id" gorm:"index"`
	FileName string `json:"file_name"` // relative to the item path, e.g. "Season 1/xxx.S01E03.mkv"
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Title    string `json:"title"`
	Overview string `json:"overview"`
	StillID  uint   `json:"still_id"`
}

// MediaAsset is an image stored on disk under <data>/media/assets, never in
// the database.
type MediaAsset struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	SHA256    string    `json:"sha256" gorm:"uniqueIndex;size:64"`
	Mime      string    `json:"mime"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	SourceURL string    `json:"source_url"` // where it came from; informational and used by import
	CreatedAt time.Time `json:"created_at"`
}

// MediaCandidate is one search hit kept for the user to confirm.
type MediaCandidate struct {
	Provider  string  `json:"provider"`
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Year      int     `json:"year"`
	PosterURL string  `json:"poster_url"`
	Score     float32 `json:"score"`
}

// MediaEpisodeIn is an episode in a set request.
type MediaEpisodeIn struct {
	FileName string `json:"file_name"`
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Title    string `json:"title"`
	Overview string `json:"overview"`
	StillID  uint   `json:"still_id"`
}

// MediaSetReq is the body of /api/admin/media/set. Only the fields present in
// the request are written; nil means "leave as it is".
type MediaSetReq struct {
	Path          string            `json:"path"`
	Kind          *string           `json:"kind,omitempty"`
	Title         *string           `json:"title,omitempty"`
	OriginalTitle *string           `json:"original_title,omitempty"`
	Year          *int              `json:"year,omitempty"`
	ExternalIDs   map[string]string `json:"external_ids,omitempty"`
	Ratings       map[string]any    `json:"ratings,omitempty"`
	Genres        []string          `json:"genres,omitempty"`
	Overview      *string           `json:"overview,omitempty"`
	PosterID      *uint             `json:"poster_id,omitempty"`
	BackdropID    *uint             `json:"backdrop_id,omitempty"`
	Locked        *bool             `json:"locked,omitempty"`
	Source        *string           `json:"source,omitempty"`
	Confidence    *float32          `json:"confidence,omitempty"`
	Candidates    []MediaCandidate  `json:"candidates,omitempty"`
	Episodes      []MediaEpisodeIn  `json:"episodes,omitempty"`
}
