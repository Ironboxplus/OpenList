package cstcloud_capsule

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/gowebdav"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// CSTCloudCapsule mounts 中国科技云·数据胶囊 (https://data.cstcloud.cn) via its
// WebDAV endpoint. The endpoint is fixed for the whole service; credentials
// are the per-space WebDAV username/password created on the client access page.
var webdavAddress = "https://data.cstcloud.cn/dav"

type CSTCloudCapsule struct {
	model.Storage
	Addition
	client *gowebdav.Client
}

func (d *CSTCloudCapsule) Config() driver.Config {
	return config
}

func (d *CSTCloudCapsule) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *CSTCloudCapsule) Init(ctx context.Context) error {
	c := gowebdav.NewClient(webdavAddress, d.Username, d.Password)
	c.SetTransport(&http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: conf.Conf.TlsInsecureSkipVerify},
	})
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	c.SetJar(jar)
	// the server gates every request on the credential's app type via
	// User-Agent; without a matching UA it responds 403 "Client type mismatch"
	c.SetInterceptor(func(method string, rq *http.Request) {
		rq.Header.Set("User-Agent", d.UserAgent)
	})
	d.client = c
	// validate credentials at mount time so misconfiguration surfaces here
	// instead of as an empty/broken listing later
	_, err = d.client.ReadDir(d.GetRootPath())
	return err
}

func (d *CSTCloudCapsule) Drop(ctx context.Context) error {
	return nil
}

func (d *CSTCloudCapsule) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.client.ReadDir(dir.GetPath())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src os.FileInfo) (model.Obj, error) {
		return &model.Object{
			Name:     src.Name(),
			Size:     src.Size(),
			Modified: src.ModTime(),
			IsFolder: src.IsDir(),
		}, nil
	})
}

func (d *CSTCloudCapsule) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	url, header, err := d.client.Link(file.GetPath())
	if err != nil {
		return nil, err
	}
	if header == nil {
		header = http.Header{}
	}
	header.Set("User-Agent", d.UserAgent)
	return &model.Link{
		URL:    url,
		Header: header,
	}, nil
}

func (d *CSTCloudCapsule) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	return d.client.MkdirAll(path.Join(parentDir.GetPath(), dirName), 0644)
}

func (d *CSTCloudCapsule) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	return d.client.Rename(getPath(srcObj), path.Join(dstDir.GetPath(), srcObj.GetName()), true)
}

func (d *CSTCloudCapsule) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	return d.client.Rename(getPath(srcObj), path.Join(path.Dir(srcObj.GetPath()), newName), true)
}

func (d *CSTCloudCapsule) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return d.client.Copy(getPath(srcObj), path.Join(dstDir.GetPath(), srcObj.GetName()), true)
}

func (d *CSTCloudCapsule) Remove(ctx context.Context, obj model.Obj) error {
	return d.client.RemoveAll(getPath(obj))
}

func (d *CSTCloudCapsule) Put(ctx context.Context, dstDir model.Obj, s model.FileStreamer, up driver.UpdateProgress) error {
	callback := func(r *http.Request) {
		r.Header.Set("Content-Type", s.GetMimetype())
		r.ContentLength = s.GetSize()
	}
	reader := driver.NewLimitedUploadStream(ctx, &driver.ReaderUpdatingProgress{
		Reader:         s,
		UpdateProgress: up,
	})
	return d.client.WriteStream(path.Join(dstDir.GetPath(), s.GetName()), reader, 0644, callback)
}

func getPath(obj model.Obj) string {
	if obj.IsDir() {
		return obj.GetPath() + "/"
	}
	return obj.GetPath()
}

var _ driver.Driver = (*CSTCloudCapsule)(nil)
