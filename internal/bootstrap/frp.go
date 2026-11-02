package bootstrap

import (
	"net/http"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/frp"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// InitFRP creates the frp manager serving handler through its loopback
// gateways, and starts the client only if the admin turned it on.
func InitFRP(handler http.Handler) {
	frp.Default = frp.New(handler)
	if err := frp.MigrateIDs(); err != nil {
		utils.Log.Warnf("frp: could not give the stored proxies their ids: %v", err)
	}
	if !setting.GetBool(conf.FRPEnabled) {
		return
	}
	if err := frp.Default.Start(); err != nil {
		utils.Log.Warnf("frp client not started: %v", err)
		return
	}
	utils.Log.Info("frp client started")
}

// StopFRP stops the client, if any.
func StopFRP() {
	if frp.Default != nil {
		frp.Default.Stop()
	}
}
