package model

// MediaSidecar is a subtitle file that sits next to a video of a work and is
// named after it ("x.mkv" and "x.chs.ass"). The scan records them because it
// lists the folders anyway; the strm storage in media mode serves them under
// the name of the virtual video without listing the source again.
//
// Both paths are relative to the place of the work: its folder for a folder
// work, the folder it sits in for a single file.
type MediaSidecar struct {
	ID     uint   `gorm:"primarykey"                                  json:"-"`
	ItemID uint   `gorm:"not null;uniqueIndex:idx_media_sidecar_file" json:"item_id"`
	Video  string `gorm:"size:191;not null;uniqueIndex:idx_media_sidecar_file" json:"video"` // the video, e.g. "Season 1/x.S01E03.mkv"
	Path   string `gorm:"size:191;not null;uniqueIndex:idx_media_sidecar_file" json:"path"`  // the subtitle, e.g. "Season 1/x.S01E03.chs.ass"
}
