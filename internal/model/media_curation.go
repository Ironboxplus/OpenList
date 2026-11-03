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

// MediaIdentifyVersion numbers the generation of the identification rules, the
// name parsing and the provider search. A work records the number it was
// scraped under (MediaCuration.IdentifyVersion); when a scan meets a scraped,
// unlocked, non-manual work from an older generation it queues the work for a
// new scrape, so a better rule reaches the works it would have read differently
// and not only new ones. Raise it whenever a change to identify, name parsing,
// scoring or search can change a verdict or a match.
//
//	1  first scan: kinds from content, versions, seasons
//	2  content-name search, original titles in scoring, firm collections
//	3  titles for members without one, recode names, nested release folders,
//	   localized titles
const MediaIdentifyVersion = 3

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
	// IdentifyVersion is the MediaIdentifyVersion the work was last scraped under (0: never).
	IdentifyVersion int `gorm:"default:0" json:"identify_version"`
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

// MediaCandidate is one search hit kept for a person to confirm.
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
	Still    string `json:"still"`
}

// MediaSetReq is the body of /api/admin/media/curate/set. Only the fields
// present in the request are written; nil means "leave as it is". Cover and
// Backdrop take the spellings MediaItem.Cover does (an http(s) URL, thumb:/...,
// a data URI).
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
	Cover         *string           `json:"cover,omitempty"`
	Backdrop      *string           `json:"backdrop,omitempty"`
	Locked        *bool             `json:"locked,omitempty"`
	Source        *string           `json:"source,omitempty"`
	Confidence    *float32          `json:"confidence,omitempty"`
	Candidates    []MediaCandidate  `json:"candidates,omitempty"`
	Episodes      []MediaEpisodeIn  `json:"episodes,omitempty"`
}
