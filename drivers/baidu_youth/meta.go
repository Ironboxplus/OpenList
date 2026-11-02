package baidu_youth

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootPath
	Cookie         string `json:"cookie" required:"true"`
	OrderBy        string `json:"order_by" type:"select" options:"name,time,size" default:"name"`
	OrderDirection string `json:"order_direction" type:"select" options:"asc,desc" default:"asc"`
	ForceProxy     bool   `json:"force_proxy" type:"bool" default:"true" help:"Proxy downloads through AList. Disable to redirect the browser to a fresh Baidu direct link."`
	DownloadAPI    string `json:"download_api" type:"select" options:"official,crack" default:"official"`
	UploadThread   string `json:"upload_thread" default:"3" help:"1<=thread<=32"`
	UploadAPI      string `json:"upload_api" default:"https://d.pcs.baidu.com"`
}

const (
	UPLOAD_FALLBACK_API              = "https://d.pcs.baidu.com"
	UPLOAD_TIMEOUT                   = time.Minute * 30
	UPLOAD_RETRY_COUNT               = 3
	UPLOAD_RETRY_WAIT_TIME           = time.Second
	UPLOAD_RETRY_MAX_WAIT_TIME       = time.Second * 5
	DefaultSliceSize           int64 = 4 * 1024 * 1024
)

var config = driver.Config{
	Name:        "BaiduYouth",
	DefaultRoot: "/",
	OnlyProxy:   true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &BaiduYouth{}
	})
}
