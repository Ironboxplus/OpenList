package _115

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	Cookie       string  `json:"cookie" type:"text" help:"one of QR code token and cookie required"`
	QRCodeToken  string  `json:"qrcode_token" type:"text" help:"one of QR code token and cookie required"`
	QRCodeSource string  `json:"qrcode_source" type:"select" options:"web,android,ios,tv,alipaymini,wechatmini,qandroid" default:"linux" help:"select the QR code device, default linux"`
	PageSize     int64   `json:"page_size" type:"number" default:"1000" help:"list api per page size of 115 driver"`
	LimitRate    float64 `json:"limit_rate" type:"float" default:"2" help:"limit all api request rate ([limit]r/1s)"`

	// A pointer: a storage saved before the field existed has none and is 2.
	MaxConnsPerFile *int `json:"max_conns_per_file" type:"number" default:"2" help:"how many download connections the 115 CDN may be asked for at once to one file (a further one is refused with 403). OpenList's own background reads of a file, such as subtitle extraction, stay within it and give way to playback. 0 = unlimited"`
	driver.RootID
}

var config = driver.Config{
	Name:          "115 Cloud",
	DefaultRoot:   "0",
	LinkCacheMode: driver.LinkCacheUA,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Pan115{}
	})
}
