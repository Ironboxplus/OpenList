package media

import (
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
)

func init() {
	// the start-up backfill of MediaItem.FileTitle reads names the way a scan does
	db.FileTitleOf = func(name string, isFile bool) string { return identify.ParseName(name, isFile).Title }
}
