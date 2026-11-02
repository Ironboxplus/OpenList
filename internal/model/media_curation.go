package model

// Video types stored in MediaItem.VideoType. The scanner and the scrapers
// decide between them; everything that treats a series differently from a
// movie must go through IsEpisodicVideo instead of comparing with "tv".
const (
	VideoTypeMovie      = "movie"
	VideoTypeTV         = "tv"
	VideoTypeAnime      = "anime"
	VideoTypeCollection = "collection"
	VideoTypeIgnore     = "ignore" // a directory the rules found to be no work (soundtracks, extras); stored hidden
)

// IsEpisodicVideo reports whether a video type is made of episodes: a show or
// an anime series. The scanner's `Season`/`Episode`/Episodes fields only apply
// to these.
func IsEpisodicVideo(videoType string) bool {
	return videoType == VideoTypeTV || videoType == VideoTypeAnime
}

// Kind names a work by what it is. The values equal the VideoType values; the
// curation code and the CLI use the Kind spelling.
const (
	MediaKindMovie      = VideoTypeMovie
	MediaKindTV         = VideoTypeTV
	MediaKindAnime      = VideoTypeAnime
	MediaKindCollection = VideoTypeCollection
	MediaKindIgnore     = VideoTypeIgnore
)

// ValidMediaKind reports whether k can be stored as the kind of a work.
func ValidMediaKind(k string) bool {
	switch k {
	case MediaKindMovie, MediaKindTV, MediaKindAnime, MediaKindCollection, MediaKindIgnore:
		return true
	}
	return false
}

// Where a work's data came from.
const (
	MediaSourceManual  = "manual"
	MediaSourceRule    = "rule"
	MediaSourceScraper = "scraper"
)

// MediaPendingConfidence is the confidence below which an unlocked work still
// needs a human look.
const MediaPendingConfidence = 0.6

// MediaCuration is what the rules, the scrapers and a human add to a MediaItem
// beyond the upstream fields. It is embedded in MediaItem, so the columns sit
// in media_items.
type MediaCuration struct {
	OriginalTitle string  `gorm:"size:255"        json:"original_title"`
	ExternalIDs   string  `gorm:"type:text"       json:"external_ids"` // JSON {"tmdb":"tv/890","imdb":"tt0112159","bgm":"265","douban":"1291843"}
	Backdrop      string  `json:"backdrop"`                            // same spellings as Cover
	Source        string  `gorm:"size:16"         json:"source"`       // manual | rule | scraper
	Confidence    float32 `json:"confidence"`                          // how sure the rules or the match are
	Locked        bool    `gorm:"default:false"   json:"locked"`       // true: scans and scrapes never overwrite the scraped fields
	Candidates    string  `gorm:"type:text"       json:"candidates"`   // JSON: top candidates when the confidence is low
}

// IsSeries reports whether a video item is a show: an episodic type, or, for an
// item no rule or scraper classified yet, any folder (the upstream scan reads a
// folder as a series).
func (m *MediaItem) IsSeries() bool {
	switch m.VideoType {
	case VideoTypeMovie, VideoTypeCollection, VideoTypeIgnore:
		return false
	case "":
		return m.IsFolder
	}
	return IsEpisodicVideo(m.VideoType)
}
