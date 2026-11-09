package _115_open

import (
	"context"
	"sort"
	"strings"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// toVideoPlayInfos maps the SDK video-play URLs to driver.VideoPlayInfo,
// sorted by Definition descending (highest quality first).
func toVideoPlayInfos(urls []sdk.VideoPlayURL) []driver.VideoPlayInfo {
	infos := make([]driver.VideoPlayInfo, 0, len(urls))
	for _, u := range urls {
		infos = append(infos, driver.VideoPlayInfo{
			Resolution: u.Desc,
			Definition: u.Definition,
			URL:        u.URL,
		})
	}
	sort.SliceStable(infos, func(i, j int) bool {
		return infos[i].Definition > infos[j].Definition
	})
	return infos
}

// embeddedSubtitlePrefix marks, in the title 115 reports, a track it extracted
// from the video container; the rest of the title is the container's own.
const embeddedSubtitlePrefix = "[内置字幕]"

// toVideoSubtitleInfos maps the SDK subtitle list to driver.VideoSubtitleInfo,
// dropping caption-map placeholder entries, any without a downloadable URL and
// any with neither a title nor a language (115 lists a blank entry after the
// real tracks). A track with a language but no title is titled by its language.
//
// An entry whose title carries embeddedSubtitlePrefix is a container track:
// EmbeddedTitle is the title without it, and EmbeddedOrder the entry's position
// among the prefixed entries in the order 115 returned them, which is the order
// of the container's subtitle streams. The position is counted before any entry
// is dropped, so a track that is dropped still holds its slot.
func toVideoSubtitleInfos(list []sdk.SubtitleItem) []driver.VideoSubtitleInfo {
	infos := make([]driver.VideoSubtitleInfo, 0, len(list))
	embedded := 0
	for _, s := range list {
		var embeddedTitle string
		var embeddedOrder *int
		if rest, ok := strings.CutPrefix(s.Title, embeddedSubtitlePrefix); ok {
			order := embedded
			embedded++
			if rest = strings.TrimSpace(rest); rest != "" {
				embeddedTitle = rest
				embeddedOrder = &order
			}
		}
		if s.URL == "" || s.IsCaptionMap == 1 || (s.Title == "" && s.Language == "") {
			continue
		}
		title := s.Title
		if title == "" {
			title = s.Language
		}
		infos = append(infos, driver.VideoSubtitleInfo{
			Language:      s.Language,
			Title:         title,
			URL:           s.URL,
			Type:          s.Type,
			EmbeddedTitle: embeddedTitle,
			EmbeddedOrder: embeddedOrder,
		})
	}
	return infos
}

// VideoSubtitle returns the provider's subtitle tracks for a video. 115 extracts
// a container's embedded (and any sidecar) subtitles during transcoding and
// serves them as standalone files, so these work on every play source —
// including the transcoded HLS streams that drop the original embedded tracks.
func (d *Open115) VideoSubtitle(ctx context.Context, file model.Obj) ([]driver.VideoSubtitleInfo, error) {
	if err := d.waitAPI(ctx, "VideoSubtitle"); err != nil {
		return nil, err
	}
	obj, ok := file.(*Obj)
	if !ok {
		return nil, errors.New("can't convert obj")
	}
	pc := obj.Pc
	if pc == "" {
		return nil, errors.New("can't get pick code")
	}

	resp, err := d.client.VideoSubtitle(ctx, pc)
	if err != nil {
		log.Errorf("[115] VideoSubtitle API failed: %v", err)
		return nil, errors.WithStack(err)
	}
	if resp == nil {
		return nil, nil
	}

	return toVideoSubtitleInfos(resp.List), nil
}

// VideoPlay returns the official online-play (transcoded streaming) sources
// for a video at multiple resolutions.
func (d *Open115) VideoPlay(ctx context.Context, file model.Obj) ([]driver.VideoPlayInfo, error) {
	if err := d.waitAPI(ctx, "VideoPlay"); err != nil {
		return nil, err
	}
	obj, ok := file.(*Obj)
	if !ok {
		return nil, errors.New("can't convert obj")
	}
	pc := obj.Pc
	if pc == "" {
		return nil, errors.New("can't get pick code")
	}

	resp, err := d.client.VideoPlay(ctx, pc)
	if err != nil {
		log.Errorf("[115] VideoPlay API failed: %v", err)
		return nil, errors.WithStack(err)
	}
	if resp == nil || len(resp.VideoURL) == 0 {
		return nil, errors.New("no official play sources")
	}

	return toVideoPlayInfos(resp.VideoURL), nil
}
