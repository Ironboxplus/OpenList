package cstcloud_capsule

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	Username string `json:"username" required:"true" help:"WebDAV username created in the data space's client access page. Note: the service currently only allows uploading .zip/.prop files over WebDAV / 在数据空间「客户端访问」中创建的 WebDAV 用户名。注意:服务端目前仅允许通过 WebDAV 上传 .zip/.prop 文件"`
	Password string `json:"password" required:"true" help:"WebDAV password shown when the credential is created / 创建凭证时显示的 WebDAV 密码"`
	// The server rejects requests whose User-Agent does not contain the app
	// type the credential was created for ("Client type mismatch"). Zotero is
	// currently the only WebDAV app type offered, so it is the default here.
	UserAgent string `json:"user_agent" required:"true" default:"Mozilla/5.0 (compatible; Zotero/8.0) AList" help:"Must contain the app type chosen when creating the credential / 必须包含创建凭证时所选的应用类型(如 Zotero)"`
	driver.RootPath
}

var config = driver.Config{
	Name:        "CSTCloudCapsule",
	LocalSort:   true,
	OnlyProxy:   true,
	DefaultRoot: "/",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &CSTCloudCapsule{}
	})
}
