package handles

import (
	"strconv"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/frp"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// secretSettingKeys are settings whose values the generic settings API never
// returns and never accepts: they are changed through /admin/frp only.
var secretSettingKeys = map[string]bool{
	conf.FRPAuthToken:    true,
	conf.FRPProxySecrets: true,
}

// maskSecretSettings blanks secret values in place.
func maskSecretSettings(items []model.SettingItem) {
	for i := range items {
		if secretSettingKeys[items[i].Key] {
			items[i].Value = ""
		}
	}
}

// dropSecretSettings removes secret keys from a generic save request.
func dropSecretSettings(items []model.SettingItem) []model.SettingItem {
	out := items[:0:0]
	for _, it := range items {
		if !secretSettingKeys[it.Key] {
			out = append(out, it)
		}
	}
	return out
}

func frpManager(c *gin.Context) *frp.Manager {
	if frp.Default == nil {
		common.ErrorStrResp(c, "frp is not available", 500)
		return nil
	}
	return frp.Default
}

// FRPProxyView is a proxy as the admin UI sees it: no secret key, only whether
// one is set.
type FRPProxyView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	CustomDomains []string `json:"custom_domains"`
	Subdomain     string   `json:"subdomain"`
	RemotePort    int      `json:"remote_port"`
	SecretSet     bool     `json:"secret_set"`
}

type FRPView struct {
	Enabled       bool           `json:"enabled"`
	ServerAddr    string         `json:"server_addr"`
	ServerPort    int            `json:"server_port"`
	TLS           bool           `json:"tls"`
	TokenSet      bool           `json:"token_set"`
	ExposeCluster bool           `json:"expose_cluster"`
	Proxies       []FRPProxyView `json:"proxies"`
	State         string         `json:"state"`
	StateMessage  string         `json:"state_message"`
}

func frpView(cfg *frp.Config, st frp.Status) FRPView {
	v := FRPView{
		Enabled: cfg.Enabled, ServerAddr: cfg.ServerAddr, ServerPort: cfg.ServerPort, TLS: cfg.TLS,
		TokenSet: cfg.Token != "", ExposeCluster: cfg.ExposeCluster, Proxies: []FRPProxyView{},
		State: st.State, StateMessage: st.StateMessage,
	}
	for _, p := range cfg.Proxies {
		doms := p.CustomDomains
		if doms == nil {
			doms = []string{}
		}
		v.Proxies = append(v.Proxies, FRPProxyView{
			ID: p.ID, Name: p.Name, Type: p.Type, CustomDomains: doms, Subdomain: p.Subdomain,
			RemotePort: p.RemotePort, SecretSet: p.SecretKey != "",
		})
	}
	return v
}

func GetFRP(c *gin.Context) {
	m := frpManager(c)
	if m == nil {
		return
	}
	cfg, err := frp.Load()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, frpView(cfg, m.Status()))
}

type frpProxyReq struct {
	// ID: the proxy's stable id as GET returned it; absent = a new proxy (or an
	// older client: matched to a stored one by name).
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	CustomDomains []string `json:"custom_domains"`
	Subdomain     string   `json:"subdomain"`
	RemotePort    int      `json:"remote_port"`
	// SecretKey: absent = keep the stored one (same id), "" = clear, value = replace.
	SecretKey *string `json:"secret_key"`
}

type frpSaveReq struct {
	Enabled       bool          `json:"enabled"`
	ServerAddr    string        `json:"server_addr"`
	ServerPort    int           `json:"server_port"`
	TLS           bool          `json:"tls"`
	Token         *string       `json:"token"` // absent = keep, "" = clear, value = replace
	ExposeCluster bool          `json:"expose_cluster"`
	Proxies       []frpProxyReq `json:"proxies"`
}

// SaveFRP validates, stores and applies the configuration. Secrets are written
// only when the request carries them.
func SaveFRP(c *gin.Context) {
	m := frpManager(c)
	if m == nil {
		return
	}
	var req frpSaveReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorStrResp(c, "invalid request", 400)
		return
	}
	old, err := frp.Load()
	if err != nil {
		old = &frp.Config{} // unreadable stored config: the request replaces it
	}
	oldByID, oldByName := map[string]frp.Proxy{}, map[string]frp.Proxy{}
	for _, p := range old.Proxies {
		if p.ID != "" {
			oldByID[p.ID] = p
		}
		oldByName[p.Name] = p
	}
	cfg := &frp.Config{
		Enabled: req.Enabled, ServerAddr: req.ServerAddr, ServerPort: req.ServerPort,
		TLS: req.TLS, Token: old.Token, ExposeCluster: req.ExposeCluster,
	}
	if cfg.ServerPort == 0 {
		cfg.ServerPort = 7000
	}
	if req.Token != nil {
		cfg.Token = *req.Token
	}
	usedIDs := map[string]bool{}
	for _, p := range req.Proxies {
		prev, known := oldByID[p.ID]
		if p.ID == "" {
			prev, known = oldByName[p.Name] // a client that does not send ids
		}
		id := ""
		if known {
			id = prev.ID
		}
		if id == "" || usedIDs[id] {
			id = frp.NewProxyID()
			prev = frp.Proxy{} // a new proxy inherits nothing
		}
		usedIDs[id] = true
		np := frp.Proxy{ID: id, Name: p.Name, Type: p.Type, CustomDomains: p.CustomDomains, Subdomain: p.Subdomain,
			RemotePort: p.RemotePort, SecretKey: prev.SecretKey}
		if p.SecretKey != nil {
			np.SecretKey = *p.SecretKey
		}
		cfg.Proxies = append(cfg.Proxies, np)
	}
	// An enabled client must be complete; a disabled one may be saved half-done.
	if cfg.Enabled {
		if err := cfg.Validate(); err != nil {
			common.ErrorStrResp(c, err.Error(), 400)
			return
		}
	}
	if err := frp.Save(cfg); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if err := m.Apply(cfg); err != nil {
		// stored, but it did not start: say why, with the status
		common.ErrorStrResp(c, m.Ring().Redact(err.Error()), 400)
		return
	}
	st := m.Status()
	common.SuccessResp(c, gin.H{"state": st.State, "state_message": st.StateMessage})
}

// StartFRP starts the client with the stored configuration, without touching
// the stored "enabled" flag.
func StartFRP(c *gin.Context) {
	m := frpManager(c)
	if m == nil {
		return
	}
	if err := m.Start(); err != nil {
		common.ErrorStrResp(c, m.Ring().Redact(err.Error()), 400)
		return
	}
	st := m.Status()
	common.SuccessResp(c, gin.H{"state": st.State, "state_message": st.StateMessage})
}

// StopFRP stops the running client (the stored "enabled" flag is unchanged: it
// starts again at the next boot).
func StopFRP(c *gin.Context) {
	m := frpManager(c)
	if m == nil {
		return
	}
	m.Stop()
	st := m.Status()
	common.SuccessResp(c, gin.H{"state": st.State, "state_message": st.StateMessage})
}

func FRPStatus(c *gin.Context) {
	m := frpManager(c)
	if m == nil {
		return
	}
	common.SuccessResp(c, m.Status())
}

func FRPRuntime(c *gin.Context) {
	m := frpManager(c)
	if m == nil {
		return
	}
	limit := 200
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = min(v, 300)
	}
	common.SuccessResp(c, gin.H{"lines": m.Runtime(limit)})
}
