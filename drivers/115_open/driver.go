package _115_open

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

type Open115 struct {
	model.Storage
	Addition
	client  *sdk.Client
	limiter *rate.Limiter
}

func (d *Open115) Config() driver.Config {
	return config
}

func (d *Open115) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Open115) Init(ctx context.Context) error {
	d.client = sdk.New(sdk.WithRefreshToken(d.Addition.RefreshToken),
		sdk.WithAccessToken(d.Addition.AccessToken),
		sdk.WithOnRefreshToken(func(s1, s2 string) {
			d.Addition.AccessToken = s1
			d.Addition.RefreshToken = s2
			op.MustSaveDriverStorage(d)
		}))
	if flags.Debug || flags.Dev {
		d.client.SetDebug(true)
	}
	_, err := d.client.UserInfo(ctx)
	if err != nil {
		return err
	}
	if d.Addition.LimitRate > 0 {
		d.limiter = rate.NewLimiter(rate.Limit(d.Addition.LimitRate), 1)
	}
	if d.PageSize <= 0 {
		d.PageSize = 200
	} else if d.PageSize > 1150 {
		d.PageSize = 1150
	}

	return nil
}

func (d *Open115) WaitLimit(ctx context.Context) error {
	if d.limiter != nil {
		return d.limiter.Wait(ctx)
	}
	return nil
}

func (d *Open115) Drop(ctx context.Context) error {
	return nil
}

func (d *Open115) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	start := time.Now()
	log.Infof("[115] List request started for dir: %s (ID: %s)", dir.GetName(), dir.GetID())

	var res []model.Obj
	pageSize := int64(d.PageSize)
	offset := int64(0)
	pageCount := 0

	for {
		if err := d.WaitLimit(ctx); err != nil {
			return nil, err
		}

		pageStart := time.Now()
		resp, err := d.client.GetFiles(ctx, &sdk.GetFilesReq{
			CID:    dir.GetID(),
			Limit:  pageSize,
			Offset: offset,
			ASC:    d.Addition.OrderDirection == "asc",
			O:      d.Addition.OrderBy,
			// Cur:     1,
			ShowDir: true,
		})
		pageDuration := time.Since(pageStart)
		pageCount++
		log.Infof("[115] GetFiles page %d took: %v (offset=%d, limit=%d)", pageCount, pageDuration, offset, pageSize)

		if err != nil {
			log.Errorf("[115] GetFiles page %d failed after %v: %v", pageCount, pageDuration, err)
			return nil, err
		}
		res = append(res, utils.MustSliceConvert(resp.Data, func(src sdk.GetFilesResp_File) model.Obj {
			obj := Obj(src)
			return &obj
		})...)
		if len(res) >= int(resp.Count) {
			break
		}
		offset += pageSize
	}

	totalDuration := time.Since(start)
	log.Infof("[115] List request completed in %v (%d pages, %d files)", totalDuration, pageCount, len(res))

	return res, nil
}

func (d *Open115) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	start := time.Now()
	log.Infof("[115] Link request started for file: %s", file.GetName())

	if err := d.WaitLimit(ctx); err != nil {
		return nil, err
	}
	var ua string
	if args.Header != nil {
		ua = args.Header.Get("User-Agent")
	}
	if ua == "" {
		ua = base.UserAgent
	}
	obj, ok := file.(*Obj)
	if !ok {
		return nil, fmt.Errorf("can't convert obj")
	}
	pc := obj.Pc

	apiStart := time.Now()
	log.Infof("[115] Calling DownURL API...")
	resp, err := d.client.DownURL(ctx, pc, ua)
	apiDuration := time.Since(apiStart)
	log.Infof("[115] DownURL API took: %v", apiDuration)

	if err != nil {
		log.Errorf("[115] DownURL API failed after %v: %v", apiDuration, err)
		return nil, err
	}
	u, ok := resp[obj.GetID()]
	if !ok {
		return nil, fmt.Errorf("can't get link")
	}

	totalDuration := time.Since(start)
	log.Infof("[115] Link request completed in %v (API: %v)", totalDuration, apiDuration)

	return &model.Link{
		URL: u.URL.URL,
		Header: http.Header{
			"User-Agent": []string{ua},
		},
	}, nil
}

func (d *Open115) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	if err := d.WaitLimit(ctx); err != nil {
		return nil, err
	}
	resp, err := d.client.Mkdir(ctx, parentDir.GetID(), dirName)
	if err != nil {
		return nil, err
	}
	return &Obj{
		Fid:  resp.FileID,
		Pid:  parentDir.GetID(),
		Fn:   dirName,
		Fc:   "0",
		Upt:  time.Now().Unix(),
		Uet:  time.Now().Unix(),
		UpPt: time.Now().Unix(),
	}, nil
}

func (d *Open115) Move(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	if err := d.WaitLimit(ctx); err != nil {
		return nil, err
	}
	_, err := d.client.Move(ctx, &sdk.MoveReq{
		FileIDs: srcObj.GetID(),
		ToCid:   dstDir.GetID(),
	})
	if err != nil {
		return nil, err
	}
	return srcObj, nil
}

func (d *Open115) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	if err := d.WaitLimit(ctx); err != nil {
		return nil, err
	}
	_, err := d.client.UpdateFile(ctx, &sdk.UpdateFileReq{
		FileID:  srcObj.GetID(),
		FileName: newName,
	})
	if err != nil {
		return nil, err
	}
	obj, ok := srcObj.(*Obj)
	if ok {
		obj.Fn = newName
	}
	return srcObj, nil
}

func (d *Open115) Copy(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	if err := d.WaitLimit(ctx); err != nil {
		return nil, err
	}
	_, err := d.client.Copy(ctx, &sdk.CopyReq{
		PID:     dstDir.GetID(),
		FileID:  srcObj.GetID(),
		NoDupli: "1",
	})
	if err != nil {
		return nil, err
	}
	return srcObj, nil
}

func (d *Open115) Remove(ctx context.Context, obj model.Obj) error {
	if err := d.WaitLimit(ctx); err != nil {
		return err
	}
	_obj, ok := obj.(*Obj)
	if !ok {
		return fmt.Errorf("can't convert obj")
	}
	_, err := d.client.DelFile(ctx, &sdk.DelFileReq{
		FileIDs:  _obj.GetID(),
		ParentID: _obj.Pid,
	})
	if err != nil {
		return err
	}
	return nil
}

func (d *Open115) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) error {
	err := d.WaitLimit(ctx)
	if err != nil {
		return err
	}

	sha1 := file.GetHash().GetHash(utils.SHA1)
	sha1128k := file.GetHash().GetHash(utils.SHA1_128K)

	// 检查是否是可重复读取的流
	_, isSeekable := file.(*stream.SeekableStream)

	// 如果有预计算的 hash，先尝试秒传
	if len(sha1) == utils.SHA1.Width && len(sha1128k) == utils.SHA1_128K.Width {
		resp, err := d.client.UploadInit(ctx, &sdk.UploadInitReq{
			FileName: file.GetName(),
			FileSize: file.GetSize(),
			Target:   dstDir.GetID(),
			FileID:   strings.ToUpper(sha1),
			PreID:    strings.ToUpper(sha1128k),
		})
		if err != nil {
			return err
		}
		if resp.Status == 2 {
			up(100)
			return nil
		}
		// 秒传失败，继续后续流程
	}

	if isSeekable {
		// 可重复读取的流，使用 RangeRead 计算 hash，不缓存
		if len(sha1) != utils.SHA1.Width {
			sha1, err = stream.StreamHashFile(file, utils.SHA1, 100, &up)
			if err != nil {
				return err
			}
		}
		// 计算 sha1_128k（如果没有预计算）
		if len(sha1128k) != utils.SHA1_128K.Width {
			const PreHashSize int64 = 128 * utils.KB
			hashSize := PreHashSize
			if file.GetSize() < PreHashSize {
				hashSize = file.GetSize()
			}
			reader, err := file.RangeRead(http_range.Range{Start: 0, Length: hashSize})
			if err != nil {
				return err
			}
			sha1128k, err = utils.HashReader(utils.SHA1, reader)
			if err != nil {
				return err
			}
		}
	} else {
		// 不可重复读取的流（如 HTTP body）
		// 如果有预计算的 hash，上面已经尝试过秒传了
		if len(sha1) == utils.SHA1.Width && len(sha1128k) == utils.SHA1_128K.Width {
			// 秒传失败，需要缓存文件进行实际上传
			_, err = file.CacheFullAndWriter(&up, nil)
			if err != nil {
				return err
			}
		} else {
			// 没有预计算的 hash，缓存整个文件并计算
			if len(sha1) != utils.SHA1.Width {
				_, sha1, err = stream.CacheFullAndHash(file, &up, utils.SHA1)
				if err != nil {
					return err
				}
			} else if file.GetFile() == nil {
				// 有 SHA1 但没有缓存，需要缓存以支持后续 RangeRead
				_, err = file.CacheFullAndWriter(&up, nil)
				if err != nil {
					return err
				}
			}
			// 计算 sha1_128k
			const PreHashSize int64 = 128 * utils.KB
			hashSize := PreHashSize
			if file.GetSize() < PreHashSize {
				hashSize = file.GetSize()
			}
			reader, err := file.RangeRead(http_range.Range{Start: 0, Length: hashSize})
			if err != nil {
				return err
			}
			sha1128k, err = utils.HashReader(utils.SHA1, reader)
			if err != nil {
				return err
			}
		}
	}

	// 1. Init（SeekableStream 或已缓存的 FileStream）
	resp, err := d.client.UploadInit(ctx, &sdk.UploadInitReq{
		FileName: file.GetName(),
		FileSize: file.GetSize(),
		Target:   dstDir.GetID(),
		FileID:   strings.ToUpper(sha1),
		PreID:    strings.ToUpper(sha1128k),
	})
	if err != nil {
		return err
	}
	if resp.Status == 2 {
		up(100)
		return nil
	}
	// 2. two way verify
	if utils.SliceContains([]int{6, 7, 8}, resp.Status) {
		signCheck := strings.Split(resp.SignCheck, "-") //"sign_check": "2392148-2392298" 取2392148-2392298之间的内容(包含2392148、2392298)的sha1
		start, err := strconv.ParseInt(signCheck[0], 10, 64)
		if err != nil {
			return err
		}
		end, err := strconv.ParseInt(signCheck[1], 10, 64)
		if err != nil {
			return err
		}
		signReader, err := file.RangeRead(http_range.Range{Start: start, Length: end - start + 1})
		if err != nil {
			return err
		}
		signVal, err := utils.HashReader(utils.SHA1, signReader)
		if err != nil {
			return err
		}
		resp, err = d.client.UploadInit(ctx, &sdk.UploadInitReq{
			FileName: file.GetName(),
			FileSize: file.GetSize(),
			Target:   dstDir.GetID(),
			FileID:   strings.ToUpper(sha1),
			PreID:    strings.ToUpper(sha1128k),
			SignKey:  resp.SignKey,
			SignVal:  strings.ToUpper(signVal),
		})
		if err != nil {
			return err
		}
		if resp.Status == 2 {
			up(100)
			return nil
		}
	}
	// 3. get upload token
	tokenResp, err := d.client.UploadGetToken(ctx)
	if err != nil {
		return err
	}
	// 4. upload
	err = d.multpartUpload(ctx, file, up, tokenResp, resp)
	if err != nil {
		return err
	}
	return nil
}

func (d *Open115) OfflineDownload(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
	return d.client.AddOfflineTaskURIs(ctx, uris, dstDir.GetID())
}

func (d *Open115) DeleteOfflineTask(ctx context.Context, infoHash string, deleteFiles bool) error {
	return d.client.DeleteOfflineTask(ctx, infoHash, deleteFiles)
}

func (d *Open115) OfflineList(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
	// 获取第一页
	resp, err := d.client.OfflineTaskList(ctx, 1)
	if err != nil {
		return nil, err
	}
	// 如果有多页，获取所有页面的任务
	if resp.PageCount > 1 {
		for page := 2; page <= resp.PageCount; page++ {
			pageResp, err := d.client.OfflineTaskList(ctx, int64(page))
			if err != nil {
				return nil, err
			}
			resp.Tasks = append(resp.Tasks, pageResp.Tasks...)
		}
	}
	return resp, nil
}

func (d *Open115) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	userInfo, err := d.client.UserInfo(ctx)
	if err != nil {
		return nil, err
	}
	total, err := ParseInt64(userInfo.RtSpaceInfo.AllTotal.Size)
	if err != nil {
		return nil, err
	}
	used, err := ParseInt64(userInfo.RtSpaceInfo.AllUse.Size)
	if err != nil {
		return nil, err
	}
	return &model.StorageDetails{
		DiskUsage: model.DiskUsage{
			TotalSpace: total,
			UsedSpace:  used,
		},
	}, nil
}

// func (d *Open115) GetArchiveMeta(ctx context.Context, obj model.Obj, args model.ArchiveArgs) (model.ArchiveMeta, error) {
// 	// TODO get archive file meta-info, return errs.NotImplement to use an internal archive tool, optional
// 	return nil, errs.NotImplement
// }

// func (d *Open115) ListArchive(ctx context.Context, obj model.Obj, args model.ArchiveInnerArgs) ([]model.Obj, error) {
// 	// TODO list args.InnerPath in the archive obj, return errs.NotImplement to use an internal archive tool, optional
// 	return nil, errs.NotImplement
// }

// func (d *Open115) Extract(ctx context.Context, obj model.Obj, args model.ArchiveInnerArgs) (*model.Link, error) {
// 	// TODO return link of file args.InnerPath in the archive obj, return errs.NotImplement to use an internal archive tool, optional
// 	return nil, errs.NotImplement
// }

// func (d *Open115) ArchiveDecompress(ctx context.Context, srcObj, dstDir model.Obj, args model.ArchiveDecompressArgs) ([]model.Obj, error) {
// 	// TODO extract args.InnerPath path in the archive srcObj to the dstDir location, optional
// 	// a folder with the same name as the archive file needs to be created to store the extracted results if args.PutIntoNewDir
// 	// return errs.NotImplement to use an internal archive tool
// 	return nil, errs.NotImplement
// }

//func (d *Template) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
//	return nil, errs.NotSupport
//}

var _ driver.Driver = (*Open115)(nil)
