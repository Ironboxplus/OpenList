package _115_open

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	// Usually one of two
	driver.RootID
	// define other
	OrderBy        string  `json:"order_by" type:"select" options:"file_name,file_size,user_utime,file_type"`
	OrderDirection string  `json:"order_direction" type:"select" options:"asc,desc"`
	RemoveWay      string  `json:"remove_way" required:"true" type:"select" options:"trash,delete" default:"trash"`
	LimitRate      float64 `json:"limit_rate" type:"float" default:"1" help:"limit all api request rate ([limit]r/1s)"`
	PageSize       int64   `json:"page_size" type:"number" default:"200" help:"list api per page size of 115open driver"`
	// A pointer: a storage saved before the field existed has none and is 2.
	MaxConnsPerFile *int `json:"max_conns_per_file" type:"number" default:"2" help:"how many download connections the 115 CDN may be asked for at once to one file (a further one is refused with 403). OpenList's own background reads of a file, such as subtitle extraction, stay within it and give way to playback. 0 = unlimited"`
	AccessToken    string  `json:"access_token" required:"true"`
	RefreshToken   string  `json:"refresh_token" required:"true"`
}

var config = driver.Config{
	Name:             "115 Open",
	DefaultRoot:      "0",
	ProxyRangeOption: true,
	LinkCacheMode:    driver.LinkCacheUA,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Open115{}
	})
}
