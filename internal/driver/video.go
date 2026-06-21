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
