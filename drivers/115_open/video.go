package _115_open

import (
	"context"
	"sort"

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

// VideoPlay returns the official online-play (transcoded streaming) sources
// for a video at multiple resolutions.
func (d *Open115) VideoPlay(ctx context.Context, file model.Obj) ([]driver.VideoPlayInfo, error) {
	if err := d.WaitLimit(ctx); err != nil {
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
