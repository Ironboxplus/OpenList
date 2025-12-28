package _123_open

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

type Open123 struct {
	model.Storage
	Addition
	UID uint64
}

func (d *Open123) Config() driver.Config {
	return config
}

func (d *Open123) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Open123) Init(ctx context.Context) error {
	if d.UploadThread < 1 || d.UploadThread > 32 {
		d.UploadThread = 3
	}

	return nil
}

func (d *Open123) Drop(ctx context.Context) error {
	op.MustSaveDriverStorage(d)
	return nil
}

func (d *Open123) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	fileLastId := int64(0)
	parentFileId, err := strconv.ParseInt(dir.GetID(), 10, 64)
	if err != nil {
		return nil, err
	}
	res := make([]File, 0)

	for fileLastId != -1 {
		files, err := d.getFiles(parentFileId, 100, fileLastId)
		if err != nil {
			return nil, err
		}
		// 目前123panAPI请求，trashed失效，只能通过遍历过滤
		for i := range files.Data.FileList {
			if files.Data.FileList[i].Trashed == 0 {
				res = append(res, files.Data.FileList[i])
			}
		}
		fileLastId = files.Data.LastFileId
	}
	return utils.SliceConvert(res, func(src File) (model.Obj, error) {
		return src, nil
	})
}

func (d *Open123) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	fileId, _ := strconv.ParseInt(file.GetID(), 10, 64)

	if d.DirectLink {
		res, err := d.getDirectLink(fileId)
		if err != nil {
			return nil, err
		}

		if d.DirectLinkPrivateKey == "" {
			duration := 365 * 24 * time.Hour // 缓存1年
			return &model.Link{
				URL:        res.Data.URL,
				Expiration: &duration,
			}, nil
		}

		uid, err := d.getUID(ctx)
		if err != nil {
			return nil, err
		}

		duration := time.Duration(d.DirectLinkValidDuration) * time.Minute

		newURL, err := d.SignURL(res.Data.URL, d.DirectLinkPrivateKey,
			uid, duration)
		if err != nil {
			return nil, err
		}

		return &model.Link{
			URL:        newURL,
			Expiration: &duration,
		}, nil
	}

	res, err := d.getDownloadInfo(fileId)
	if err != nil {
		return nil, err
	}

	return &model.Link{URL: res.Data.DownloadUrl}, nil
}

func (d *Open123) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	parentFileId, _ := strconv.ParseInt(parentDir.GetID(), 10, 64)

	return d.mkdir(parentFileId, dirName)
}

func (d *Open123) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	toParentFileID, _ := strconv.ParseInt(dstDir.GetID(), 10, 64)

	return d.move(srcObj.(File).FileId, toParentFileID)
}

func (d *Open123) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	fileId, _ := strconv.ParseInt(srcObj.GetID(), 10, 64)

	return d.rename(fileId, newName)
}

func (d *Open123) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	// 尝试使用上传+MD5秒传功能实现复制
	// 1. 创建文件
	// parentFileID 父目录id，上传到根目录时填写 0
	parentFileId, err := strconv.ParseInt(dstDir.GetID(), 10, 64)
	if err != nil {
		return fmt.Errorf("parse parentFileID error: %v", err)
	}
	etag := srcObj.(File).Etag
	createResp, err := d.create(parentFileId, srcObj.GetName(), etag, srcObj.GetSize(), 2, false)
	if err != nil {
		return err
	}
	// 是否秒传
	if createResp.Data.Reuse {
		return nil
	}
	return errs.NotSupport
}

func (d *Open123) Remove(ctx context.Context, obj model.Obj) error {
	fileId, _ := strconv.ParseInt(obj.GetID(), 10, 64)

	return d.trash(fileId)
}

func (d *Open123) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	utils.Log.Debugf("╔══════════════════════════════════════════════════════════╗")
	utils.Log.Debugf("║ [123网盘Put] 开始上传文件")
	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
	utils.Log.Debugf("║ 文件名: %s", file.GetName())
	utils.Log.Debugf("║ 文件大小: %d bytes", file.GetSize())
	utils.Log.Debugf("║ 目标目录ID: %s", dstDir.GetID())

	// 1. 准备参数
	// parentFileID 父目录id，上传到根目录时填写 0
	parentFileId, err := strconv.ParseInt(dstDir.GetID(), 10, 64)
	if err != nil {
		utils.Log.Errorf("║ ✗ 解析父目录ID失败: %v", err)
		utils.Log.Debugf("╚══════════════════════════════════════════════════════════╝")
		return nil, fmt.Errorf("parse parentFileID error: %v", err)
	}
	utils.Log.Debugf("║ 父目录ID(int64): %d", parentFileId)

	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
	utils.Log.Debugf("║ [步骤1] 获取文件MD5(etag)")

	// etag 文件md5
	etag := file.GetHash().GetHash(utils.MD5)
	utils.Log.Debugf("║ 从FileStreamer获取的MD5: '%s'", etag)
	utils.Log.Debugf("║ MD5长度: %d", len(etag))
	utils.Log.Debugf("║ MD5.Width要求: %d", utils.MD5.Width)
	utils.Log.Debugf("║ 长度检查: len(%d) < Width(%d) = %v", len(etag), utils.MD5.Width, len(etag) < utils.MD5.Width)

	// 验证etag是否是有效的十六进制字符串
	if etag != "" {
		if _, err := hex.DecodeString(etag); err == nil {
			utils.Log.Debugf("║ ✓ 当前MD5是有效的十六进制字符串")
		} else {
			utils.Log.Warnf("║ ✗ 当前MD5不是有效的十六进制: %v", err)
		}
	}

	if len(etag) < utils.MD5.Width {
		utils.Log.Debugf("║ ⚠ MD5长度不足，需要流式计算真实MD5")
		utils.Log.Debugf("║ 开始调用 stream.CacheFullAndHash...")
		utils.Log.Debugf("║ 这将:")
		utils.Log.Debugf("║   1. 缓存整个文件到本地临时文件")
		utils.Log.Debugf("║   2. 同时计算文件的真实MD5")
		utils.Log.Debugf("║   3. 返回 hex.EncodeToString(md5Hash.Sum(nil))")

		cachedFile, newEtag, err := stream.CacheFullAndHash(file, &up, utils.MD5)
		if err != nil {
			utils.Log.Errorf("║ ✗ 计算MD5失败: %v", err)
			utils.Log.Debugf("╚══════════════════════════════════════════════════════════╝")
			return nil, err
		}

		utils.Log.Debugf("║ ✓ 计算完成")
		utils.Log.Debugf("║ 旧MD5: '%s'", etag)
		utils.Log.Debugf("║ 新MD5: '%s'", newEtag)
		utils.Log.Debugf("║ 缓存文件: %v (类型: %T)", cachedFile != nil, cachedFile)

		// 验证新MD5格式
		if decoded, err := hex.DecodeString(newEtag); err == nil {
			utils.Log.Debugf("║ ✓ 新MD5是有效的十六进制字符串 (decoded length: %d)", len(decoded))
		} else {
			utils.Log.Errorf("║ ✗ 新MD5不是有效的十六进制: %v", err)
		}

		etag = newEtag
		utils.Log.Debugf("║ 使用新计算的MD5: '%s'", etag)
	} else {
		utils.Log.Debugf("║ ✓ MD5长度符合要求，直接使用")
		utils.Log.Debugf("║ 当前MD5: '%s'", etag)
		utils.Log.Warnf("║")
		utils.Log.Warnf("║ ⚠⚠⚠ 关键问题：此MD5来自源网盘！⚠⚠⚠")
		utils.Log.Warnf("║")
		utils.Log.Warnf("║ 如果源是百度网盘:")
		utils.Log.Warnf("║   百度返回: 加密MD5 (如 'b0ea37602n80afe104da316baece0b3c')")
		utils.Log.Warnf("║   经过解密: 标准MD5 (如 'ae2b620eb1c972072765c6d305f9740c')")
		utils.Log.Warnf("║   但是！解密后的MD5 ≠ 文件真实MD5")
		utils.Log.Warnf("║   这只是百度的内部文件标识符")
		utils.Log.Warnf("║")
		utils.Log.Warnf("║ 后果:")
		utils.Log.Warnf("║   使用错误MD5上传到123网盘")
		utils.Log.Warnf("║   → 123服务器计算文件真实MD5")
		utils.Log.Warnf("║   → 发现与提交的MD5不匹配")
		utils.Log.Warnf("║   → complete失败: '服务端的哈希和之前提交的不同'")
		utils.Log.Warnf("║")
		utils.Log.Warnf("║ 解决方案: 强制重新计算文件真实MD5")
		utils.Log.Warnf("║")

		// 测试：强制重新计算真实MD5进行对比
		utils.Log.Warnf("║ [测试] 强制计算文件真实MD5进行对比...")
		savedEtag := etag
		cachedFile, realEtag, err := stream.CacheFullAndHash(file, &up, utils.MD5)
		if err != nil {
			utils.Log.Errorf("║ ✗ 计算真实MD5失败: %v", err)
		} else {
			utils.Log.Warnf("║ 对比结果:")
			utils.Log.Warnf("║   源网盘提供的MD5: %s", savedEtag)
			utils.Log.Warnf("║   文件真实MD5:     %s", realEtag)
			utils.Log.Warnf("║   是否一致: %v", savedEtag == realEtag)
			if savedEtag != realEtag {
				utils.Log.Errorf("║")
				utils.Log.Errorf("║ ✗✗✗ 确认：源网盘MD5是错误的！✗✗✗")
				utils.Log.Errorf("║")
				utils.Log.Errorf("║ 这就是导致上传失败的根本原因！")
				utils.Log.Errorf("║ 必须使用真实计算的MD5: %s", realEtag)
				utils.Log.Errorf("║")
				etag = realEtag // 使用真实MD5
			} else {
				utils.Log.Debugf("║ ✓ 源网盘MD5是正确的，可以直接使用")
				etag = savedEtag
			}
			utils.Log.Debugf("║ 缓存文件: %v", cachedFile != nil)
		}
	}

	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
	utils.Log.Debugf("║ [步骤2] 调用create接口")
	utils.Log.Debugf("║ 参数:")
	utils.Log.Debugf("║   parentFileId: %d", parentFileId)
	utils.Log.Debugf("║   filename: %s", file.GetName())
	utils.Log.Debugf("║   etag: %s", etag)
	utils.Log.Debugf("║   size: %d", file.GetSize())
	utils.Log.Debugf("║   duplicate: 2")
	utils.Log.Debugf("║   containDir: false")

	createResp, err := d.create(parentFileId, file.GetName(), etag, file.GetSize(), 2, false)
	if err != nil {
		utils.Log.Errorf("║ ✗ create调用失败: %v", err)
		utils.Log.Debugf("╚══════════════════════════════════════════════════════════╝")
		return nil, err
	}
	utils.Log.Debugf("║ ✓ create调用成功")
	utils.Log.Debugf("║ 响应: Reuse=%v, FileID=%d, PreuploadID=%s",
		createResp.Data.Reuse, createResp.Data.FileID, createResp.Data.PreuploadID)

	// 是否秒传
	if createResp.Data.Reuse {
		utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
		utils.Log.Debugf("║ [秒传] 文件已存在，可以秒传")
		// 秒传成功才会返回正确的 FileID，否则为 0
		if createResp.Data.FileID != 0 {
			utils.Log.Debugf("║ ✓ 秒传成功！FileID: %d", createResp.Data.FileID)
			utils.Log.Debugf("╚══════════════════════════════════════════════════════════╝")
			return File{
				FileName: file.GetName(),
				Size:     file.GetSize(),
				FileId:   createResp.Data.FileID,
				Type:     2,
				Etag:     etag,
			}, nil
		} else {
			utils.Log.Warnf("║ ⚠ Reuse=true但FileID=0，继续正常上传流程")
		}
	} else {
		utils.Log.Debugf("║ 不能秒传，需要上传分片")
	}

	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
	utils.Log.Debugf("║ [步骤3] 上传分片")
	// 2. 上传分片
	err = d.Upload(ctx, file, createResp, up)
	if err != nil {
		utils.Log.Errorf("║ ✗ 上传分片失败: %v", err)
		utils.Log.Debugf("╚══════════════════════════════════════════════════════════╝")
		return nil, err
	}
	utils.Log.Debugf("║ ✓ 所有分片上传完成")

	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
	utils.Log.Debugf("║ [步骤4] 等待complete确认 (最多60秒)")
	utils.Log.Debugf("║ PreuploadID: %s", createResp.Data.PreuploadID)

	// 3. 上传完毕
	for i := range 60 {
		uploadCompleteResp, err := d.complete(createResp.Data.PreuploadID)

		if i == 0 || i%5 == 0 {
			utils.Log.Debugf("║ [轮询%d/60] 调用complete...", i+1)
		}

		if err != nil {
			utils.Log.Warnf("║ [轮询%d/60] complete返回错误: %v", i+1, err)
		} else {
			utils.Log.Debugf("║ [轮询%d/60] 响应: Completed=%v, FileID=%d",
				i+1, uploadCompleteResp.Data.Completed, uploadCompleteResp.Data.FileID)
		}

		// 返回错误代码未知，如：20103，文档也没有具体说
		if err == nil && uploadCompleteResp.Data.Completed && uploadCompleteResp.Data.FileID != 0 {
			utils.Log.Debugf("║ ✓ 上传完成确认成功！")
			utils.Log.Debugf("║ 最终FileID: %d", uploadCompleteResp.Data.FileID)
			utils.Log.Debugf("╚══════════════════════════════════════════════════════════╝")
			up(100)
			return File{
				FileName: file.GetName(),
				Size:     file.GetSize(),
				FileId:   uploadCompleteResp.Data.FileID,
				Type:     2,
				Etag:     etag,
			}, nil
		}

		// 若接口返回的completed为 false 时，则需间隔1秒继续轮询此接口，获取上传最终结果。
		time.Sleep(time.Second)
	}

	utils.Log.Errorf("║ ✗ complete超时！60秒后仍未确认完成")
	utils.Log.Debugf("╚══════════════════════════════════════════════════════════╝")
	return nil, fmt.Errorf("upload complete timeout")
}

func (d *Open123) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	userInfo, err := d.getUserInfo(ctx)
	if err != nil {
		return nil, err
	}
	total := userInfo.Data.SpacePermanent + userInfo.Data.SpaceTemp
	free := total - userInfo.Data.SpaceUsed
	return &model.StorageDetails{
		DiskUsage: model.DiskUsage{
			TotalSpace: total,
			FreeSpace:  free,
		},
	}, nil
}

func (d *Open123) OfflineDownload(ctx context.Context, url string, dir model.Obj, callback string) (int, error) {
	return d.createOfflineDownloadTask(ctx, url, dir.GetID(), callback)
}

func (d *Open123) OfflineDownloadProcess(ctx context.Context, taskID int) (float64, int, error) {
	return d.queryOfflineDownloadStatus(ctx, taskID)
}

var (
	_ driver.Driver    = (*Open123)(nil)
	_ driver.PutResult = (*Open123)(nil)
)
