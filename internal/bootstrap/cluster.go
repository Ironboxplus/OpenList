package bootstrap

import (
	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/internal/cluster"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// InitClusterSync loads the storage-sharing cluster engine: identity, config and
// persisted CRDT state, and registers the storage hook so local credential
// changes propagate to peers. The network loops are started later (after storages
// load) by StartClusterSync. Failure is non-fatal — the server runs fine without
// cluster sync.
func InitClusterSync() {
	if _, err := cluster.Init(flags.DataDir); err != nil {
		utils.Log.Warnf("[cluster] init failed, storage sync disabled: %v", err)
		return
	}
	utils.Log.Infof("[cluster] node %s ready", cluster.Default.NodeID())
}

// StartClusterSync seeds records from the now-loaded local storages and starts
// the anti-entropy loop. No-op when cluster sync was not initialized.
func StartClusterSync() {
	if cluster.Default == nil {
		return
	}
	cluster.Default.Start()
}
