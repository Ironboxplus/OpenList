package baidu_netdisk

import (
	"context"
	"errors"
	"net/url"
	stdpath "path"
	"strconv"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	streamPkg "github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
)

type BaiduNetdisk struct {
	model.Storage
	Addition

	uploadThread int
	vipType      int // 会员类型，0普通用户(4G/4M)、1普通会员(10G/16M)、2超级会员(20G/32M)
}

var ErrUploadIDExpired = errors.New("uploadid expired")
var ErrUploadURLExpired = errors.New("upload url expired or unavailable")

func (d *BaiduNetdisk) Config() driver.Config {
	return config
}

func (d *BaiduNetdisk) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *BaiduNetdisk) Init(ctx context.Context) error {
	d.uploadThread, _ = strconv.Atoi(d.UploadThread)
	if d.uploadThread < 1 {
		d.uploadThread, d.UploadThread = 1, "1"
	} else if d.uploadThread > 32 {
		d.uploadThread, d.UploadThread = 32, "32"
	}

	if _, err := url.Parse(d.UploadAPI); d.UploadAPI == "" || err != nil {
		d.UploadAPI = UPLOAD_FALLBACK_API
	}

	res, err := d.get("/xpan/nas", map[string]string{
		"method": "uinfo",
	}, nil)
	log.Debugf("[baidu_netdisk] get uinfo: %s", string(res))
	if err != nil {
		return err
	}
	d.vipType = utils.Json.Get(res, "vip_type").ToInt()
	return nil
}

func (d *BaiduNetdisk) Drop(ctx context.Context) error {
	return nil
}

func (d *BaiduNetdisk) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(dir.GetPath())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *BaiduNetdisk) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	switch d.DownloadAPI {
	case "crack":
		return d.linkCrack(file, args)
	case "crack_video":
		return d.linkCrackVideo(file, args)
	}
	return d.linkOfficial(file, args)
}

func (d *BaiduNetdisk) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	var newDir File
	_, err := d.create(stdpath.Join(parentDir.GetPath(), dirName), 0, 1, "", "", &newDir, 0, 0)
	if err != nil {
		return nil, err
	}
	return fileToObj(newDir), nil
}

func (d *BaiduNetdisk) Move(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	data := []base.Json{
		{
			"path":    srcObj.GetPath(),
			"dest":    dstDir.GetPath(),
			"newname": srcObj.GetName(),
		},
	}
	_, err := d.manage("move", data)
	if err != nil {
		return nil, err
	}
	if srcObj, ok := srcObj.(*model.ObjThumb); ok {
		srcObj.SetPath(stdpath.Join(dstDir.GetPath(), srcObj.GetName()))
		srcObj.Modified = time.Now()
		return srcObj, nil
	}
	return nil, nil
}

func (d *BaiduNetdisk) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	data := []base.Json{
		{
			"path":    srcObj.GetPath(),
			"newname": newName,
		},
	}
	_, err := d.manage("rename", data)
	if err != nil {
		return nil, err
	}

	if srcObj, ok := srcObj.(*model.ObjThumb); ok {
		srcObj.SetPath(stdpath.Join(stdpath.Dir(srcObj.GetPath()), newName))
		srcObj.Name = newName
		srcObj.Modified = time.Now()
		return srcObj, nil
	}
	return nil, nil
}

func (d *BaiduNetdisk) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	data := []base.Json{
		{
			"path":    srcObj.GetPath(),
			"dest":    dstDir.GetPath(),
			"newname": srcObj.GetName(),
		},
	}
	_, err := d.manage("copy", data)
	return err
}

func (d *BaiduNetdisk) Remove(ctx context.Context, obj model.Obj) error {
	data := []string{obj.GetPath()}
	_, err := d.manage("delete", data)
	return err
}

func (d *BaiduNetdisk) PutRapid(ctx context.Context, dstDir model.Obj, stream model.FileStreamer) (model.Obj, error) {
	contentMd5 := stream.GetHash().GetHash(utils.MD5)
	if len(contentMd5) < utils.MD5.Width {
		return nil, errors.New("invalid hash")
	}

	streamSize := stream.GetSize()
	path := stdpath.Join(dstDir.GetPath(), stream.GetName())
	mtime := stream.ModTime().Unix()
	ctime := stream.CreateTime().Unix()
	blockList, _ := utils.Json.MarshalToString([]string{contentMd5})

	var newFile File
	_, err := d.create(path, streamSize, 0, "", blockList, &newFile, mtime, ctime)
	if err != nil {
		return nil, err
	}
	// 修复时间，具体原因见 Put 方法注释的 **注意**
	newFile.Ctime = stream.CreateTime().Unix()
	newFile.Mtime = stream.ModTime().Unix()
	return fileToObj(newFile), nil
}

// Put
//
// **注意**: 截至 2024/04/20 百度云盘 api 接口返回的时间永远是当前时间，而不是文件时间。
// 而实际上云盘存储的时间是文件时间，所以此处需要覆盖时间，保证缓存与云盘的数据一致
func (d *BaiduNetdisk) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	// 百度网盘不允许上传空文件
	if stream.GetSize() < 1 {
		return nil, ErrBaiduEmptyFilesNotAllowed
	}

	// rapid upload
	if newObj, err := d.PutRapid(ctx, dstDir, stream); err == nil {
		return newObj, nil
	}

	streamSize := stream.GetSize()
	sliceSize := d.getSliceSize(streamSize)
	count := 1
	if streamSize > sliceSize {
		count = int((streamSize + sliceSize - 1) / sliceSize)
	}

	path := stdpath.Join(dstDir.GetPath(), stream.GetName())
	mtime := stream.ModTime().Unix()
	ctime := stream.CreateTime().Unix()

	// step.1 流式计算MD5哈希值（使用 RangeRead，不会消耗流）
	contentMd5, sliceMd5, blockList, err := d.calculateHashesStream(ctx, stream, sliceSize, &up)
	if err != nil {
		return nil, err
	}

	blockListStr, _ := utils.Json.MarshalToString(blockList)

	// step.2 尝试读取已保存进度或执行预上传
	precreateResp, ok := base.GetUploadProgress[*PrecreateResp](d, d.AccessToken, contentMd5)
	if !ok {
		// 没有进度，走预上传
		precreateResp, err = d.precreate(ctx, path, streamSize, blockListStr, contentMd5, sliceMd5, ctime, mtime)
		if err != nil {
			return nil, err
		}
		if precreateResp.ReturnType == 2 {
			// rapid upload, since got md5 match from baidu server
			// 修复时间，具体原因见 Put 方法注释的 **注意**
			precreateResp.File.Ctime = ctime
			precreateResp.File.Mtime = mtime
			return fileToObj(precreateResp.File), nil
		}
	}

	ensureUploadURL := func() {
		if precreateResp.UploadURL != "" {
			return
		}
		precreateResp.UploadURL = d.getUploadUrl(path, precreateResp.Uploadid)
	}

	// step.3 流式上传分片
	// 创建 StreamSectionReader 用于上传
	ss, err := streamPkg.NewStreamSectionReader(stream, int(sliceSize), &up)
	if err != nil {
		return nil, err
	}

uploadLoop:
	for range 2 {
		// 获取上传域名
		ensureUploadURL()

		// 流式并发上传
		err = d.uploadChunksStream(ctx, ss, stream, precreateResp, path, sliceSize, count, up)
		if err == nil {
			break uploadLoop
		}

		// 保存进度（所有错误都会保存）
		precreateResp.BlockList = utils.SliceFilter(precreateResp.BlockList, func(s int) bool { return s >= 0 })
		base.SaveUploadProgress(d, precreateResp, d.AccessToken, contentMd5)

		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if errors.Is(err, ErrUploadIDExpired) {
			log.Warn("[baidu_netdisk] uploadid expired, will restart from scratch")
			// 重新 precreate（所有分片都要重传）
			newPre, err2 := d.precreate(ctx, path, streamSize, blockListStr, "", "", ctime, mtime)
			if err2 != nil {
				return nil, err2
			}
			if newPre.ReturnType == 2 {
				return fileToObj(newPre.File), nil
			}
			precreateResp = newPre
			precreateResp.UploadURL = ""
			// 覆盖掉旧的进度
			base.SaveUploadProgress(d, precreateResp, d.AccessToken, contentMd5)

			// 尝试重新创建 StreamSectionReader（如果流支持重新读取）
			ss, err = streamPkg.NewStreamSectionReader(stream, int(sliceSize), &up)
			if err != nil {
				return nil, err
			}
			continue uploadLoop
		}
		return nil, err
	}
	defer up(100)

	// step.4 创建文件
	var newFile File
	_, err = d.create(path, streamSize, 0, precreateResp.Uploadid, blockListStr, &newFile, mtime, ctime)
	if err != nil {
		return nil, err
	}
	// 修复时间，具体原因见 Put 方法注释的 **注意**
	newFile.Ctime = ctime
	newFile.Mtime = mtime
	// 上传成功清理进度
	base.SaveUploadProgress(d, nil, d.AccessToken, contentMd5)
	return fileToObj(newFile), nil
}

// precreate 执行预上传操作，支持首次上传和 uploadid 过期重试
func (d *BaiduNetdisk) precreate(ctx context.Context, path string, streamSize int64, blockListStr, contentMd5, sliceMd5 string, ctime, mtime int64) (*PrecreateResp, error) {
	params := map[string]string{"method": "precreate"}
	form := map[string]string{
		"path":       path,
		"size":       strconv.FormatInt(streamSize, 10),
		"isdir":      "0",
		"autoinit":   "1",
		"rtype":      "3",
		"block_list": blockListStr,
	}

	// 只有在首次上传时才包含 content-md5 和 slice-md5
	if contentMd5 != "" && sliceMd5 != "" {
		form["content-md5"] = contentMd5
		form["slice-md5"] = sliceMd5
	}

	joinTime(form, ctime, mtime)

	var precreateResp PrecreateResp
	_, err := d.postForm("/xpan/file", params, form, &precreateResp)
	if err != nil {
		return nil, err
	}

	// 修复时间，具体原因见 Put 方法注释的 **注意**
	if precreateResp.ReturnType == 2 {
		precreateResp.File.Ctime = ctime
		precreateResp.File.Mtime = mtime
	}

	return &precreateResp, nil
}

func (d *BaiduNetdisk) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	du, err := d.quota(ctx)
	if err != nil {
		return nil, err
	}
	return &model.StorageDetails{DiskUsage: du}, nil
}

var _ driver.Driver = (*BaiduNetdisk)(nil)
