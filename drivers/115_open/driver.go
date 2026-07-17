package _115_open

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	stdpath "path"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
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
	client     *sdk.Client
	limiter    *rate.Limiter
	parentPath string
	// tokenInvalid is the edge-trigger latch for the cluster validity protocol:
	// set once the refresh_token is found dead, cleared once a request succeeds
	// again. It debounces the token-valid/invalid notifications down to real
	// transitions instead of firing them on every request.
	tokenInvalid atomic.Bool
}

var (
	// 回收站列表存在短暂最终一致性延迟，永久删除 fallback 查找增加短重试。
	recycleBinLookupMaxAttempts = 4
	recycleBinLookupRetryDelay  = 300 * time.Millisecond
	new115SDKClient             = sdk.New
)

func (d *Open115) Config() driver.Config {
	return config
}

func (d *Open115) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Open115) Init(ctx context.Context) error {
	d.client = new115SDKClient(sdk.WithRefreshToken(d.Addition.RefreshToken),
		sdk.WithAccessToken(d.Addition.AccessToken),
		sdk.WithOnRefreshToken(func(s1, s2 string) {
			d.Addition.AccessToken = s1
			d.Addition.RefreshToken = s2
			op.MustSaveDriverStorage(d)
		}),
		// Cluster token-validity protocol: a node only shares a token it has
		// proven valid, and pulls a fresh one from a healthy peer when its own
		// dies. Edge-triggered so the cluster only reacts to real transitions.
		sdk.WithOnTokenValid(func() {
			if d.shouldNotifyTokenValid() {
				op.NotifyStorageTokenValid(d)
			}
		}),
		sdk.WithOnTokenInvalid(func() {
			if d.tokenInvalid.CompareAndSwap(false, true) {
				op.NotifyStorageTokenInvalid(d)
			}
		}))
	applySDKProxyIfConfigured(d.client)
	if flags.Debug || flags.Dev {
		d.client.SetDebug(true)
	}
	d.initLimiter()
	if err := d.WaitLimit(ctx); err != nil {
		return err
	}
	_, err := d.client.UserInfo(ctx)
	if err != nil {
		return err
	}
	if d.PageSize <= 0 {
		d.PageSize = 200
	} else if d.PageSize > 1150 {
		d.PageSize = 1150
	}

	// add parent path
	d.parentPath = "/"
	if d.GetRootId() != d.Config().DefaultRoot {
		if err := d.WaitLimit(ctx); err != nil {
			return err
		}
		folderInfo, err := d.client.GetFolderInfo(ctx, d.GetRootId())
		if err != nil {
			return err
		}

		if folderInfo.FileID != d.Config().DefaultRoot {
			d.parentPath = stdpath.Join(d.parentPath, folderInfo.FileName)
		}

		parentPaths := folderInfo.Paths
		slices.Reverse(parentPaths)
		for _, parentPathInfo := range parentPaths {
			if parentPathInfo.FileID == d.Config().DefaultRoot {
				d.parentPath = stdpath.Join("/", d.parentPath)
			} else {
				d.parentPath = stdpath.Join("/", parentPathInfo.FileName, d.parentPath)
			}
		}
	}
	return nil
}

func (d *Open115) shouldNotifyTokenValid() bool {
	if d.tokenInvalid.CompareAndSwap(true, false) {
		return true
	}
	return d.GetStorage().Status != op.WORK
}

func (d *Open115) initLimiter() {
	if d.Addition.LimitRate > 0 {
		d.limiter = rate.NewLimiter(rate.Limit(d.Addition.LimitRate), 1)
		return
	}
	d.limiter = nil
}

func applySDKProxyIfConfigured(client *sdk.Client) {
	if client == nil || conf.Conf == nil || strings.TrimSpace(conf.Conf.ProxyAddress) == "" {
		return
	}
	proxyAddress := strings.TrimSpace(conf.Conf.ProxyAddress)
	if _, err := url.Parse(proxyAddress); err != nil {
		log.Warnf("[115] invalid proxy address ignored: %v", err)
		return
	}
	client.SetProxy(proxyAddress)
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

	link := &model.Link{
		URL: u.URL.URL,
		Header: http.Header{
			"User-Agent": []string{ua},
		},
	}
	// Tie the cache TTL to the CDN's own `t=` expiry so OP never serves a
	// URL that 115's CDN has already invalidated. Without this, OP would
	// hand out a dead URL and 115 responds with 200 + Content-Length: 0,
	// which downstream clients see as a corrupt/empty stream.
	if ttl, ok := parseCDNExpiry(u.URL.URL); ok {
		link.Expiration = &ttl
	}
	return link, nil
}

func (d *Open115) Get(ctx context.Context, path string) (model.Obj, error) {
	if err := d.WaitLimit(ctx); err != nil {
		return nil, err
	}
	path = stdpath.Join(d.parentPath, path)
	resp, err := d.client.GetFolderInfoByPath(ctx, path)
	if err != nil {
		if isObjectNotFound(err) {
			return nil, errs.ObjectNotFound
		}
		return nil, err
	}
	log.Debugf("[115] GetFolderInfoByPath(%s) => Size=%q FileCategory=%q FileID=%s",
		path, resp.Size, resp.FileCategory, resp.FileID)
	return fromFolderInfo(resp)
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
		FileID:   srcObj.GetID(),
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
	resp, err := d.client.DelFile(ctx, &sdk.DelFileReq{
		FileIDs:  _obj.GetID(),
		ParentID: _obj.Pid,
	})
	if err != nil {
		return err
	}
	if d.RemoveWay != "delete" {
		return nil
	}
	return d.removePermanently(ctx, _obj, resp)
}

func (d *Open115) removePermanently(ctx context.Context, obj *Obj, deleteResp []string) error {
	var directDeleteErr error
	for _, tid := range deleteResp {
		tid = strings.TrimSpace(tid)
		if tid == "" {
			continue
		}
		if err := d.deleteRecycleBinEntry(ctx, tid); err == nil {
			return nil
		} else if directDeleteErr == nil {
			directDeleteErr = err
		}
	}

	recycleEntry, err := d.findRecycleBinEntryWithRetry(ctx, obj)
	if err != nil {
		if directDeleteErr != nil {
			return fmt.Errorf("failed to permanently delete recycle-bin candidate: %w; fallback lookup failed: %v", directDeleteErr, err)
		}
		return err
	}
	if err := d.deleteRecycleBinEntry(ctx, recycleEntry.ID); err != nil {
		if directDeleteErr != nil {
			return fmt.Errorf("failed to permanently delete recycle-bin entry %s after candidate delete error %v: %w", recycleEntry.ID, directDeleteErr, err)
		}
		return err
	}
	return nil
}

func (d *Open115) deleteRecycleBinEntry(ctx context.Context, tid string) error {
	if err := d.WaitLimit(ctx); err != nil {
		return err
	}
	_, err := d.client.RbDelete(ctx, tid)
	return err
}

func (d *Open115) findRecycleBinEntry(ctx context.Context, obj *Obj) (*sdk.RbListResp_FileInfo, error) {
	pageSize := d.PageSize
	if pageSize <= 0 {
		pageSize = 200
	} else if pageSize > 1150 {
		pageSize = 1150
	}

	offset := int64(0)
	for {
		if err := d.WaitLimit(ctx); err != nil {
			return nil, err
		}
		resp, err := d.client.RbList(ctx, pageSize, offset)
		if err != nil {
			return nil, err
		}
		if entry := matchRecycleBinEntry(obj, resp.Files); entry != nil {
			return entry, nil
		}

		count, err := strconv.ParseInt(resp.Count, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse recycle bin count %q: %w", resp.Count, err)
		}
		offset += pageSize
		if offset >= count || len(resp.Files) == 0 {
			break
		}
	}

	return nil, fmt.Errorf("recycle bin entry not found for object id=%s name=%s parent=%s", obj.GetID(), obj.GetName(), obj.Pid)
}

func isRecycleBinEntryNotFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "recycle bin entry not found")
}

func (d *Open115) findRecycleBinEntryWithRetry(ctx context.Context, obj *Obj) (*sdk.RbListResp_FileInfo, error) {
	attempts := recycleBinLookupMaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		entry, err := d.findRecycleBinEntry(ctx, obj)
		if err == nil {
			return entry, nil
		}

		lastErr = err
		if !isRecycleBinEntryNotFoundErr(err) || i == attempts-1 {
			break
		}

		wait := recycleBinLookupRetryDelay * time.Duration(i+1)
		if wait <= 0 {
			continue
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	return nil, lastErr
}

func matchRecycleBinEntry(obj *Obj, files map[string]sdk.RbListResp_FileInfo) *sdk.RbListResp_FileInfo {
	if len(files) == 0 {
		return nil
	}
	if entry, ok := files[obj.GetID()]; ok {
		matched := entry
		return &matched
	}

	size := strconv.FormatInt(obj.GetSize(), 10)
	for _, entry := range files {
		if entry.ID == obj.GetID() {
			matched := entry
			return &matched
		}
		cid := string(entry.CID)
		if obj.IsDir() {
			if entry.FileName == obj.GetName() && cid == obj.Pid {
				matched := entry
				return &matched
			}
			continue
		}
		if obj.Sha1 != "" && entry.SHA1 != "" && strings.EqualFold(entry.SHA1, obj.Sha1) {
			if entry.FileName == obj.GetName() || cid == obj.Pid {
				matched := entry
				return &matched
			}
		}
		if entry.FileName == obj.GetName() && cid == obj.Pid && entry.FileSize == size {
			matched := entry
			return &matched
		}
	}
	return nil
}

func (d *Open115) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) error {
	var err error
	sha1 := file.GetHash().GetHash(utils.SHA1)
	sha1128k := file.GetHash().GetHash(utils.SHA1_128K)

	// 检查是否是可重复读取的流
	_, isSeekable := file.(*stream.SeekableStream)

	// 如果有预计算的 hash，先尝试秒传
	if len(sha1) == utils.SHA1.Width && len(sha1128k) == utils.SHA1_128K.Width {
		if err := d.WaitLimit(ctx); err != nil {
			return err
		}
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
	if err := d.WaitLimit(ctx); err != nil {
		return err
	}
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
		if err := d.WaitLimit(ctx); err != nil {
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
	if err := d.WaitLimit(ctx); err != nil {
		return err
	}
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

func (d *Open115) OfflineDownloadWithDetails(ctx context.Context, uris []string, dstDir model.Obj) ([]string, []sdk.AddOfflineTaskURIsResp, string, error) {
	var envelope sdk.Resp[[]sdk.AddOfflineTaskURIsResp]
	response, err := d.client.AuthRequestRaw(ctx, sdk.ApiAddOffline, http.MethodPost, nil, sdk.ReqWithForm(sdk.Form{
		"urls":       strings.Join(uris, "\n"),
		"wp_path_id": dstDir.GetID(),
	}))
	if response != nil {
		_ = json.Unmarshal(response.Bytes(), &envelope)
	}
	hashes := make([]string, 0, len(envelope.Data))
	for _, item := range envelope.Data {
		if item.State && item.InfoHash != "" {
			hashes = append(hashes, item.InfoHash)
		}
	}
	rawResponse := ""
	if response != nil {
		rawResponse = response.String()
	}
	return hashes, envelope.Data, rawResponse, err
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
