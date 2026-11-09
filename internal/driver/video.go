package driver

import (
	"context"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// VideoPlayInfo describes one official transcoded online-play source for a video.
type VideoPlayInfo struct {
	Resolution string `json:"resolution"` // human readable, e.g. "1080P"
	Definition int    `json:"definition"`
	URL        string `json:"url"`
}

// VideoPlayer is an optional driver capability that exposes the storage
// provider's official online-play (transcoded streaming) sources for a video.
type VideoPlayer interface {
	VideoPlay(ctx context.Context, file model.Obj) ([]VideoPlayInfo, error)
}

// VideoSubtitleInfo describes one external subtitle track for a video, exposed
// by providers that index/extract subtitles independently of the media stream
// (e.g. 115 extracts a container's embedded subtitle tracks during transcoding
// and serves them as standalone files). Because they are independent of the
// play source, they render on every quality tier — including transcoded HLS
// streams that don't carry the original container's embedded subtitles.
type VideoSubtitleInfo struct {
	Language string `json:"language"` // provider language code, e.g. "chi", "eng"
	Title    string `json:"title"`    // human label, e.g. "简体中文"
	URL      string `json:"url"`
	Type     string `json:"type"` // subtitle format: "srt" | "ass" | "vtt"
	// Source tells where the track came from: "sidecar" (a subtitle file found
	// next to the video) or "provider" (reported by the storage provider's API).
	Source string `json:"source"`
	// EmbeddedTitle / EmbeddedOrder are set only for a track the provider
	// extracted from the video container itself: the container's own track
	// title, and the track's position among the container's subtitle streams.
	// They let a player treat the extracted file as the same logical subtitle
	// as the in-band track of the original file. Absent for sidecar files and
	// for provider tracks that are not container tracks.
	EmbeddedTitle string `json:"embedded_title,omitempty"`
	EmbeddedOrder *int   `json:"embedded_order,omitempty"`
}

// VideoSubtitleProvider is an optional driver capability exposing the provider's
// subtitle tracks for a video, independent of the play source.
type VideoSubtitleProvider interface {
	VideoSubtitle(ctx context.Context, file model.Obj) ([]VideoSubtitleInfo, error)
}
