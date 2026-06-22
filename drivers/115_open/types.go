package _115_open

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	sdk "github.com/OpenListTeam/115-sdk-go"
)

type Obj sdk.GetFilesResp_File

// Thumb implements model.Thumb.
func (o *Obj) Thumb() string {
	return o.Thumbnail
}

// CreateTime implements model.Obj.
func (o *Obj) CreateTime() time.Time {
	return time.Unix(o.UpPt, 0)
}

// GetHash implements model.Obj.
func (o *Obj) GetHash() utils.HashInfo {
	return utils.NewHashInfo(utils.SHA1, o.Sha1)
}

// GetID implements model.Obj.
func (o *Obj) GetID() string {
	return o.Fid
}

// GetName implements model.Obj.
func (o *Obj) GetName() string {
	return o.Fn
}

// GetPath implements model.Obj.
func (o *Obj) GetPath() string {
	return ""
}

// GetSize implements model.Obj.
func (o *Obj) GetSize() int64 {
	return o.FS
}

// IsDir implements model.Obj.
func (o *Obj) IsDir() bool {
	return o.Fc == "0"
}

// ModTime implements model.Obj.
func (o *Obj) ModTime() time.Time {
	return time.Unix(o.Upt, 0)
}

// Extra surfaces 115-specific metadata the universal Obj cannot carry: media
// duration (seconds), video resolution label, the starred flag and file tags.
// Only non-empty values are included, so the frontend renders whatever is
// present and ignores the rest — adding/removing keys never breaks the client,
// and a changed 115 payload degrades to "no extra info" rather than an error.
func (o *Obj) Extra() map[string]any {
	extra := map[string]any{}
	if o.Ism == "1" {
		extra["starred"] = true
	}
	if d := o.durationSec(); d > 0 {
		extra["duration"] = d
	}
	if label := defLabel(o.maxDef()); label != "" {
		extra["resolution"] = label
	}
	if len(o.Fl) > 0 {
		tags := make([]string, 0, len(o.Fl))
		for _, t := range o.Fl {
			if t.Name != "" {
				tags = append(tags, t.Name)
			}
		}
		if len(tags) > 0 {
			extra["tags"] = tags
		}
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}

// durationSec parses 115's play_long (json.Number, seconds) defensively.
func (o *Obj) durationSec() float64 {
	if o.PlayLong == "" {
		return 0
	}
	f, err := o.PlayLong.Float64()
	if err != nil || f <= 0 {
		return 0
	}
	return f
}

// maxDef prefers the higher of the two resolution fields 115 reports.
func (o *Obj) maxDef() int64 {
	if o.Def2 > o.Def {
		return o.Def2
	}
	return o.Def
}

// defLabel maps 115's video-definition code to a short badge. Unknown codes
// (including 0 for non-videos) return "" so no badge is shown.
func defLabel(def int64) string {
	switch def {
	case 1:
		return "SD"
	case 2:
		return "HD"
	case 3:
		return "FHD"
	case 4:
		return "1080P"
	case 5:
		return "4K"
	case 100:
		return "原画"
	default:
		return ""
	}
}

var _ model.Obj = (*Obj)(nil)
var _ model.Thumb = (*Obj)(nil)
var _ model.ObjExtra = (*Obj)(nil)
