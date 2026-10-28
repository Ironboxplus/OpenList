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
	Ack          string `json:"ack" type:"select" options:"staged,durable" default:"staged" help:"staged: a small file is acknowledged once it is on the local disk (state dir) and goes into a pack in the background, so packs fill up however few uploads run at once; only losing the local disk before the upload loses acknowledged data. durable: acknowledged when its pack is on the underlying storage."`
	StageMax     string `json:"stage_max_bytes" default:"1GiB" help:"Most data waiting in the staging area; uploads wait while it is full."`
	PackMax      string `json:"pack_max_bytes" default:"128MiB" help:"Seal a pack when it reaches this size."`
	SealQuiet    string `json:"seal_quiet" help:"Seal the open pack when no small file arrived for this long. Empty: 30s (staged) or 2s (durable)."`
	SealMax      string `json:"seal_max" help:"Seal the open pack this long after its first file. Empty: 10m (staged) or 15s (durable)."`
	DirCache     string `json:"dir_cache" default:"5m" help:"How long listings of the underlying storage are trusted."`
	GCGrace      string `json:"gc_grace" default:"24h" help:"Grace before unreferenced packs and old log segments are deleted."`
	LeaseTTL     string `json:"lease_ttl" default:"30m" help:"A writer whose heartbeat is older than this is considered dead and may be replaced. The heartbeat (one read and one write of the lease on the underlying storage) runs every third of it."`
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
