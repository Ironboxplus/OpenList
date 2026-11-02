package darkibox

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	APIKey string `json:"api_key" required:"true" help:"API key from your Darkibox account"`
}

var config = driver.Config{
	Name:        "Darkibox",
	LocalSort:   false,
	OnlyProxy:   true,
	NoCache:     false,
	NoUpload:    false,
	DefaultRoot: "0",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Darkibox{}
	})
}
