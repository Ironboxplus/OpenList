package openlist

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

type OpenList struct {
	model.Storage
	Addition
}

func (d *OpenList) Config() driver.Config {
	return config
}

func (d *OpenList) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *OpenList) Init(ctx context.Context) error {
	d.Addition.Address = strings.TrimSuffix(d.Addition.Address, "/")
	var resp common.Resp[MeResp]
	_, _, err := d.request("/me", http.MethodGet, func(req *resty.Request) {
		req.SetResult(&resp)
	})
	if err != nil {
		return err
	}
	// if the username is not empty and the username is not the same as the current username, then login again
	if d.Username != resp.Data.Username {
		err = d.login()
		if err != nil {
			return err
		}
	}
	// re-get the user info
	_, _, err = d.request("/me", http.MethodGet, func(req *resty.Request) {
		req.SetResult(&resp)
	})
	if err != nil {
		return err
	}
	if resp.Data.Role == model.GUEST {
		u := d.Address + "/api/public/settings"
		res, err := base.RestyClient.R().Get(u)
		if err != nil {
			return err
		}
		allowMounted := utils.Json.Get(res.Body(), "data", conf.AllowMounted).ToString() == "true"
		if !allowMounted {
			return fmt.Errorf("the site does not allow mounted")
		}
	}
	return err
}

func (d *OpenList) Drop(ctx context.Context) error {
	return nil
}

func (d *OpenList) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	var resp common.Resp[FsListResp]
	_, _, err := d.request("/fs/list", http.MethodPost, func(req *resty.Request) {
		req.SetResult(&resp).SetBody(ListReq{
			PageReq: model.PageReq{
				Page:    1,
				PerPage: 0,
			},
			Path:     dir.GetPath(),
			Password: d.MetaPassword,
			Refresh:  false,
		})
	})
	if err != nil {
		return nil, err
	}
	var files []model.Obj
	for _, f := range resp.Data.Content {
		file := model.ObjThumb{
			Object: model.Object{
				Name:     f.Name,
				Path:     path.Join(dir.GetPath(), f.Name),
				Modified: f.Modified,
				Ctime:    f.Created,
				Size:     f.Size,
				IsFolder: f.IsDir,
				HashInfo: utils.FromString(f.HashInfo),
			},
			Thumbnail: model.Thumbnail{Thumbnail: f.Thumb},
		}
		files = append(files, &file)
	}
	return files, nil
}

func (d *OpenList) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	var resp common.Resp[FsGetResp]
	headers := map[string]string{
		"User-Agent": base.UserAgent,
	}
	// if PassUAToUpsteam is true, then pass the user-agent to the upstream
	if d.PassUAToUpsteam {
		userAgent := args.Header.Get("user-agent")
		if userAgent != "" {
			headers["User-Agent"] = userAgent
		}
	}
	// if PassIPToUpsteam is true, then pass the ip address to the upstream
	if d.PassIPToUpsteam {
		ip := args.IP
		if ip != "" {
			headers["X-Forwarded-For"] = ip
			headers["X-Real-Ip"] = ip
		}
	}
	_, _, err := d.request("/fs/get", http.MethodPost, func(req *resty.Request) {
		req.SetResult(&resp).SetBody(FsGetReq{
			Path:     file.GetPath(),
			Password: d.MetaPassword,
		}).SetHeaders(headers)
	})
	if err != nil {
		return nil, err
	}
	return &model.Link{
		URL: resp.Data.RawURL,
	}, nil
}

func (d *OpenList) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	_, _, err := d.request("/fs/mkdir", http.MethodPost, func(req *resty.Request) {
		req.SetBody(MkdirOrLinkReq{
			Path: path.Join(parentDir.GetPath(), dirName),
		})
	})
	return err
}

func (d *OpenList) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	_, _, err := d.request("/fs/move", http.MethodPost, func(req *resty.Request) {
		req.SetBody(MoveCopyReq{
			SrcDir: path.Dir(srcObj.GetPath()),
			DstDir: dstDir.GetPath(),
			Names:  []string{srcObj.GetName()},
		})
	})
	return err
}

func (d *OpenList) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	_, _, err := d.request("/fs/rename", http.MethodPost, func(req *resty.Request) {
		req.SetBody(RenameReq{
			Path: srcObj.GetPath(),
			Name: newName,
		})
	})
	return err
}

func (d *OpenList) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	_, _, err := d.request("/fs/copy", http.MethodPost, func(req *resty.Request) {
		req.SetBody(MoveCopyReq{
			SrcDir: path.Dir(srcObj.GetPath()),
			DstDir: dstDir.GetPath(),
			Names:  []string{srcObj.GetName()},
		})
	})
	return err
}

func (d *OpenList) Remove(ctx context.Context, obj model.Obj) error {
	_, _, err := d.request("/fs/remove", http.MethodPost, func(req *resty.Request) {
		req.SetBody(RemoveReq{
			Dir:   path.Dir(obj.GetPath()),
			Names: []string{obj.GetName()},
		})
	})
	return err
}

func (d *OpenList) Put(ctx context.Context, dstDir model.Obj, s model.FileStreamer, up driver.UpdateProgress) error {
	// 预计算 hash（如果不存在），使用 RangeRead 不消耗 Reader
	// 这样远端驱动不需要再计算，避免 HTTP body 被重复读取
	md5Hash := s.GetHash().GetHash(utils.MD5)
	sha1Hash := s.GetHash().GetHash(utils.SHA1)
	sha256Hash := s.GetHash().GetHash(utils.SHA256)
	sha1_128kHash := s.GetHash().GetHash(utils.SHA1_128K)
	preHash := s.GetHash().GetHash(utils.PRE_HASH)

	// 计算所有缺失的 hash，确保最大兼容性
	if len(md5Hash) != utils.MD5.Width {
		var err error
		md5Hash, err = stream.StreamHashFile(s, utils.MD5, 33, &up)
		if err != nil {
			log.Warnf("[openlist] failed to pre-calculate MD5: %v", err)
			md5Hash = ""
		}
	}
	if len(sha1Hash) != utils.SHA1.Width {
		var err error
		sha1Hash, err = stream.StreamHashFile(s, utils.SHA1, 33, &up)
		if err != nil {
			log.Warnf("[openlist] failed to pre-calculate SHA1: %v", err)
			sha1Hash = ""
		}
	}
	if len(sha256Hash) != utils.SHA256.Width {
		var err error
		sha256Hash, err = stream.StreamHashFile(s, utils.SHA256, 34, &up)
		if err != nil {
			log.Warnf("[openlist] failed to pre-calculate SHA256: %v", err)
			sha256Hash = ""
		}
	}

	// 计算特殊 hash（用于秒传验证）
	// SHA1_128K: 前128KB的SHA1，115网盘使用
	if len(sha1_128kHash) != utils.SHA1_128K.Width {
		const PreHashSize int64 = 128 * 1024 // 128KB
		hashSize := PreHashSize
		if s.GetSize() < PreHashSize {
			hashSize = s.GetSize()
		}
		reader, err := s.RangeRead(http_range.Range{Start: 0, Length: hashSize})
		if err == nil {
			sha1_128kHash, err = utils.HashReader(utils.SHA1, reader)
			if closer, ok := reader.(io.Closer); ok {
				_ = closer.Close()
			}
			if err != nil {
				log.Warnf("[openlist] failed to pre-calculate SHA1_128K: %v", err)
				sha1_128kHash = ""
			}
		} else {
			log.Warnf("[openlist] failed to RangeRead for SHA1_128K: %v", err)
		}
	}

	// PRE_HASH: 前1024字节的SHA1，阿里云盘使用
	if len(preHash) != utils.PRE_HASH.Width {
		const PreHashSize int64 = 1024 // 1KB
		hashSize := PreHashSize
		if s.GetSize() < PreHashSize {
			hashSize = s.GetSize()
		}
		reader, err := s.RangeRead(http_range.Range{Start: 0, Length: hashSize})
		if err == nil {
			preHash, err = utils.HashReader(utils.SHA1, reader)
			if closer, ok := reader.(io.Closer); ok {
				_ = closer.Close()
			}
			if err != nil {
				log.Warnf("[openlist] failed to pre-calculate PRE_HASH: %v", err)
				preHash = ""
			}
		} else {
			log.Warnf("[openlist] failed to RangeRead for PRE_HASH: %v", err)
		}
	}

	// 诊断日志：检查流的状态
	if ss, ok := s.(*stream.SeekableStream); ok {
		if ss.Reader != nil {
			log.Warnf("[openlist] WARNING: SeekableStream.Reader is not nil for file %s, stream may have been consumed!", s.GetName())
		}
	}

	reader := driver.NewLimitedUploadStream(ctx, &driver.ReaderUpdatingProgress{
		Reader:         s,
		UpdateProgress: up,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, d.Address+"/api/fs/put", reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", d.Token)
	req.Header.Set("File-Path", path.Join(dstDir.GetPath(), s.GetName()))
	req.Header.Set("Password", d.MetaPassword)
	if len(md5Hash) > 0 {
		req.Header.Set("X-File-Md5", md5Hash)
	}
	if len(sha1Hash) > 0 {
		req.Header.Set("X-File-Sha1", sha1Hash)
	}
	if len(sha256Hash) > 0 {
		req.Header.Set("X-File-Sha256", sha256Hash)
	}
	if len(sha1_128kHash) > 0 {
		req.Header.Set("X-File-Sha1-128k", sha1_128kHash)
	}
	if len(preHash) > 0 {
		req.Header.Set("X-File-Pre-Hash", preHash)
	}

	req.ContentLength = s.GetSize()
	// client := base.NewHttpClient()
	// client.Timeout = time.Hour * 6
	res, err := base.HttpClient.Do(req)
	if err != nil {
		return err
	}

	bytes, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	log.Debugf("[openlist] response body: %s", string(bytes))
	if res.StatusCode >= 400 {
		return fmt.Errorf("request failed, status: %s", res.Status)
	}
	code := utils.Json.Get(bytes, "code").ToInt()
	if code != 200 {
		if code == 401 || code == 403 {
			err = d.login()
			if err != nil {
				return err
			}
		}
		return fmt.Errorf("request failed,code: %d, message: %s", code, utils.Json.Get(bytes, "message").ToString())
	}
	return nil
}

func (d *OpenList) GetArchiveMeta(ctx context.Context, obj model.Obj, args model.ArchiveArgs) (model.ArchiveMeta, error) {
	if !d.ForwardArchiveReq {
		return nil, errs.NotImplement
	}
	var resp common.Resp[ArchiveMetaResp]
	_, code, err := d.request("/fs/archive/meta", http.MethodPost, func(req *resty.Request) {
		req.SetResult(&resp).SetBody(ArchiveMetaReq{
			ArchivePass: args.Password,
			Password:    d.MetaPassword,
			Path:        obj.GetPath(),
			Refresh:     false,
		})
	})
	if code == 202 {
		return nil, errs.WrongArchivePassword
	}
	if err != nil {
		return nil, err
	}
	var tree []model.ObjTree
	if resp.Data.Content != nil {
		tree = make([]model.ObjTree, 0, len(resp.Data.Content))
		for _, content := range resp.Data.Content {
			tree = append(tree, &content)
		}
	}
	return &model.ArchiveMetaInfo{
		Comment:   resp.Data.Comment,
		Encrypted: resp.Data.Encrypted,
		Tree:      tree,
	}, nil
}

func (d *OpenList) ListArchive(ctx context.Context, obj model.Obj, args model.ArchiveInnerArgs) ([]model.Obj, error) {
	if !d.ForwardArchiveReq {
		return nil, errs.NotImplement
	}
	var resp common.Resp[ArchiveListResp]
	_, code, err := d.request("/fs/archive/list", http.MethodPost, func(req *resty.Request) {
		req.SetResult(&resp).SetBody(ArchiveListReq{
			ArchiveMetaReq: ArchiveMetaReq{
				ArchivePass: args.Password,
				Password:    d.MetaPassword,
				Path:        obj.GetPath(),
				Refresh:     false,
			},
			PageReq: model.PageReq{
				Page:    1,
				PerPage: 0,
			},
			InnerPath: args.InnerPath,
		})
	})
	if code == 202 {
		return nil, errs.WrongArchivePassword
	}
	if err != nil {
		return nil, err
	}
	var files []model.Obj
	for _, f := range resp.Data.Content {
		file := model.ObjThumb{
			Object: model.Object{
				Name:     f.Name,
				Modified: f.Modified,
				Ctime:    f.Created,
				Size:     f.Size,
				IsFolder: f.IsDir,
				HashInfo: utils.FromString(f.HashInfo),
			},
			Thumbnail: model.Thumbnail{Thumbnail: f.Thumb},
		}
		files = append(files, &file)
	}
	return files, nil
}

func (d *OpenList) Extract(ctx context.Context, obj model.Obj, args model.ArchiveInnerArgs) (*model.Link, error) {
	if !d.ForwardArchiveReq {
		return nil, errs.NotSupport
	}
	var resp common.Resp[ArchiveMetaResp]
	_, _, err := d.request("/fs/archive/meta", http.MethodPost, func(req *resty.Request) {
		req.SetResult(&resp).SetBody(ArchiveMetaReq{
			ArchivePass: args.Password,
			Password:    d.MetaPassword,
			Path:        obj.GetPath(),
			Refresh:     false,
		})
	})
	if err != nil {
		return nil, err
	}
	return &model.Link{
		URL: fmt.Sprintf("%s?inner=%s&pass=%s&sign=%s",
			resp.Data.RawURL,
			utils.EncodePath(args.InnerPath, true),
			url.QueryEscape(args.Password),
			resp.Data.Sign),
	}, nil
}

func (d *OpenList) ArchiveDecompress(ctx context.Context, srcObj, dstDir model.Obj, args model.ArchiveDecompressArgs) error {
	if !d.ForwardArchiveReq {
		return errs.NotImplement
	}
	dir, name := path.Split(srcObj.GetPath())
	_, _, err := d.request("/fs/archive/decompress", http.MethodPost, func(req *resty.Request) {
		req.SetBody(DecompressReq{
			ArchivePass:   args.Password,
			CacheFull:     args.CacheFull,
			DstDir:        dstDir.GetPath(),
			InnerPath:     args.InnerPath,
			Name:          []string{name},
			PutIntoNewDir: args.PutIntoNewDir,
			SrcDir:        dir,
			Overwrite:     args.Overwrite,
		})
	})
	return err
}

func (d *OpenList) ResolveLinkCacheMode(_ string) driver.LinkCacheMode {
	var mode driver.LinkCacheMode
	if d.PassIPToUpsteam {
		mode |= driver.LinkCacheIP
	}
	if d.PassUAToUpsteam {
		mode |= driver.LinkCacheUA
	}
	return mode
}

var _ driver.Driver = (*OpenList)(nil)
