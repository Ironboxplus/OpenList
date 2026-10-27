package cfs

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// Addition is the configuration of a cfs storage: a filesystem that keeps
// small files in packs and big ones 1:1 on another OpenList storage.
type Addition struct {
	driver.RootPath
	RemotePath string `json:"remote_path" required:"true" help:"Mount path of the storage folder that holds the volume, e.g. /115/cfs. Must not be inside this storage."`
	StateDir   string `json:"state_dir" help:"Local directory of the index. Default: <data>/cfs/<storage id>. Losing it is safe, the index is rebuilt from the drive."`

	Threshold    string `json:"threshold" default:"8MiB" help:"Files smaller than this are packed; others are stored 1:1."`
	MetaInflight int    `json:"meta_inflight" default:"1" help:"Simultaneous metadata calls (list, stat, remove, move) to the underlying storage."`
	Transfers    int    `json:"transfers" default:"8" help:"Simultaneous transfers (uploads, ranged reads) to the underlying storage. The parts of one big file stay sequential."`
	SealQuiet    string `json:"seal_quiet" default:"2s" help:"Seal the open pack when no small file arrived for this long."`
	SealMax      string `json:"seal_max" default:"15s" help:"Seal the open pack this long after its first file."`
	DirCache     string `json:"dir_cache" default:"5m" help:"How long listings of the underlying storage are trusted."`
	GCGrace      string `json:"gc_grace" default:"24h" help:"Grace before unreferenced packs and old log segments are deleted."`
}

var config = driver.Config{
	Name:        "Cfs",
	LocalSort:   true,
	OnlyProxy:   true,
	NoCache:     true,
	DefaultRoot: "/",
	NoLinkURL:   true,
	CheckStatus: true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Cfs{}
	})
}
