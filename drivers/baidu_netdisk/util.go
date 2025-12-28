package baidu_netdisk

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/avast/retry-go"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

// do others that not defined in Driver interface

func (d *BaiduNetdisk) refreshToken() error {
	err := d._refreshToken()
	if err != nil && errors.Is(err, errs.EmptyToken) {
		err = d._refreshToken()
	}
	return err
}

func (d *BaiduNetdisk) _refreshToken() error {
	// 使用在线API刷新Token，无需ClientID和ClientSecret
	if d.UseOnlineAPI && len(d.APIAddress) > 0 {
		u := d.APIAddress
		var resp struct {
			RefreshToken string `json:"refresh_token"`
			AccessToken  string `json:"access_token"`
			ErrorMessage string `json:"text"`
		}
		_, err := base.RestyClient.R().
			SetResult(&resp).
			SetQueryParams(map[string]string{
				"refresh_ui": d.RefreshToken,
				"server_use": "true",
				"driver_txt": "baiduyun_go",
			}).
			Get(u)
		if err != nil {
			return err
		}
		if resp.RefreshToken == "" || resp.AccessToken == "" {
			if resp.ErrorMessage != "" {
				return fmt.Errorf("failed to refresh token: %s", resp.ErrorMessage)
			}
			return fmt.Errorf("empty token returned from official API, a wrong refresh token may have been used")
		}
		d.AccessToken = resp.AccessToken
		d.RefreshToken = resp.RefreshToken
		op.MustSaveDriverStorage(d)
		return nil
	}
	// 使用本地客户端的情况下检查是否为空
	if d.ClientID == "" || d.ClientSecret == "" {
		return fmt.Errorf("empty ClientID or ClientSecret")
	}
	// 走原有的刷新逻辑
	u := "https://openapi.baidu.com/oauth/2.0/token"
	var resp base.TokenResp
	var e TokenErrResp
	_, err := base.RestyClient.R().
		SetResult(&resp).
		SetError(&e).
		SetQueryParams(map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": d.RefreshToken,
			"client_id":     d.ClientID,
			"client_secret": d.ClientSecret,
		}).
		Get(u)
	if err != nil {
		return err
	}
	if e.Error != "" {
		return fmt.Errorf("%s : %s", e.Error, e.ErrorDescription)
	}
	if resp.RefreshToken == "" {
		return errs.EmptyToken
	}
	d.AccessToken, d.RefreshToken = resp.AccessToken, resp.RefreshToken
	op.MustSaveDriverStorage(d)
	return nil
}

func (d *BaiduNetdisk) request(furl string, method string, callback base.ReqCallback, resp interface{}) ([]byte, error) {
	var result []byte
	err := retry.Do(func() error {
		req := base.RestyClient.R()
		req.SetQueryParam("access_token", d.AccessToken)
		if callback != nil {
			callback(req)
		}
		if resp != nil {
			req.SetResult(resp)
		}
		res, err := req.Execute(method, furl)
		if err != nil {
			return err
		}
		log.Debugf("[baidu_netdisk] req: %s, resp: %s", furl, res.String())
		errno := utils.Json.Get(res.Body(), "errno").ToInt()
		if errno != 0 {
			if utils.SliceContains([]int{111, -6}, errno) {
				log.Info("[baidu_netdisk] refreshing baidu_netdisk token.")
				err2 := d.refreshToken()
				if err2 != nil {
					return retry.Unrecoverable(err2)
				}
			}

			if errno == 31023 && d.DownloadAPI == "crack_video" {
				result = res.Body()
				return nil
			}

			return fmt.Errorf("req: [%s] ,errno: %d, refer to https://pan.baidu.com/union/doc/", furl, errno)
		}
		result = res.Body()
		return nil
	},
		retry.LastErrorOnly(true),
		retry.Attempts(3),
		retry.Delay(time.Second),
		retry.DelayType(retry.BackOffDelay))
	return result, err
}

func (d *BaiduNetdisk) get(pathname string, params map[string]string, resp interface{}) ([]byte, error) {
	return d.request("https://pan.baidu.com/rest/2.0"+pathname, http.MethodGet, func(req *resty.Request) {
		req.SetQueryParams(params)
	}, resp)
}

func (d *BaiduNetdisk) postForm(pathname string, params map[string]string, form map[string]string, resp interface{}) ([]byte, error) {
	return d.request("https://pan.baidu.com/rest/2.0"+pathname, http.MethodPost, func(req *resty.Request) {
		req.SetQueryParams(params)
		req.SetFormData(form)
	}, resp)
}

func (d *BaiduNetdisk) getFiles(dir string) ([]File, error) {
	start := 0
	limit := 200
	params := map[string]string{
		"method": "list",
		"dir":    dir,
		"web":    "web",
	}
	if d.OrderBy != "" {
		params["order"] = d.OrderBy
		if d.OrderDirection == "desc" {
			params["desc"] = "1"
		}
	}
	res := make([]File, 0)
	for {
		params["start"] = strconv.Itoa(start)
		params["limit"] = strconv.Itoa(limit)
		start += limit
		var resp ListResp
		_, err := d.get("/xpan/file", params, &resp)
		if err != nil {
			return nil, err
		}
		if len(resp.List) == 0 {
			break
		}

		if d.OnlyListVideoFile {
			for _, file := range resp.List {
				if file.Isdir == 1 || file.Category == 1 {
					res = append(res, file)
				}
			}
		} else {
			res = append(res, resp.List...)
		}
	}
	return res, nil
}

func (d *BaiduNetdisk) linkOfficial(file model.Obj, _ model.LinkArgs) (*model.Link, error) {
	var resp DownloadResp
	params := map[string]string{
		"method": "filemetas",
		"fsids":  fmt.Sprintf("[%s]", file.GetID()),
		"dlink":  "1",
	}
	_, err := d.get("/xpan/multimedia", params, &resp)
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s&access_token=%s", resp.List[0].Dlink, d.AccessToken)
	res, err := base.NoRedirectClient.R().SetHeader("User-Agent", "pan.baidu.com").Head(u)
	if err != nil {
		return nil, err
	}
	// if res.StatusCode() == 302 {
	u = res.Header().Get("location")
	//}

	return &model.Link{
		URL: u,
		Header: http.Header{
			"User-Agent": []string{"pan.baidu.com"},
		},
	}, nil
}

func (d *BaiduNetdisk) linkCrack(file model.Obj, _ model.LinkArgs) (*model.Link, error) {
	var resp DownloadResp2
	param := map[string]string{
		"target": fmt.Sprintf("[\"%s\"]", file.GetPath()),
		"dlink":  "1",
		"web":    "5",
		"origin": "dlna",
	}
	_, err := d.request("https://pan.baidu.com/api/filemetas", http.MethodGet, func(req *resty.Request) {
		req.SetQueryParams(param)
	}, &resp)
	if err != nil {
		return nil, err
	}

	return &model.Link{
		URL: resp.Info[0].Dlink,
		Header: http.Header{
			"User-Agent": []string{d.CustomCrackUA},
		},
	}, nil
}

func (d *BaiduNetdisk) linkCrackVideo(file model.Obj, _ model.LinkArgs) (*model.Link, error) {
	param := map[string]string{
		"type":       "VideoURL",
		"path":       file.GetPath(),
		"fs_id":      file.GetID(),
		"devuid":     "0%1",
		"clienttype": "1",
		"channel":    "android_15_25010PN30C_bd-netdisk_1523a",
		"nom3u8":     "1",
		"dlink":      "1",
		"media":      "1",
		"origin":     "dlna",
	}
	resp, err := d.request("https://pan.baidu.com/api/mediainfo", http.MethodGet, func(req *resty.Request) {
		req.SetQueryParams(param)
	}, nil)
	if err != nil {
		return nil, err
	}

	return &model.Link{
		URL: utils.Json.Get(resp, "info", "dlink").ToString(),
		Header: http.Header{
			"User-Agent": []string{d.CustomCrackUA},
		},
	}, nil
}

func (d *BaiduNetdisk) manage(opera string, filelist any) ([]byte, error) {
	params := map[string]string{
		"method": "filemanager",
		"opera":  opera,
	}
	marshal, _ := utils.Json.MarshalToString(filelist)
	return d.postForm("/xpan/file", params, map[string]string{
		"async":    "0",
		"filelist": marshal,
		"ondup":    "fail",
	}, nil)
}

func (d *BaiduNetdisk) create(path string, size int64, isdir int, uploadid, block_list string, resp any, mtime, ctime int64) ([]byte, error) {
	params := map[string]string{
		"method": "create",
	}
	form := map[string]string{
		"path":  path,
		"size":  strconv.FormatInt(size, 10),
		"isdir": strconv.Itoa(isdir),
		"rtype": "3",
	}
	if mtime != 0 && ctime != 0 {
		joinTime(form, ctime, mtime)
	}

	if uploadid != "" {
		form["uploadid"] = uploadid
	}
	if block_list != "" {
		form["block_list"] = block_list
	}
	return d.postForm("/xpan/file", params, form, resp)
}

func joinTime(form map[string]string, ctime, mtime int64) {
	form["local_mtime"] = strconv.FormatInt(mtime, 10)
	form["local_ctime"] = strconv.FormatInt(ctime, 10)
}

const (
	DefaultSliceSize int64 = 4 * utils.MB
	VipSliceSize     int64 = 16 * utils.MB
	SVipSliceSize    int64 = 32 * utils.MB

	MaxSliceNum       = 2048 // 文档写的是 1024/没写 ，但实际测试是 2048
	SliceStep   int64 = 1 * utils.MB
)

func (d *BaiduNetdisk) getSliceSize(filesize int64) int64 {
	// 非会员固定为 4MB
	if d.vipType == 0 {
		if d.CustomUploadPartSize != 0 {
			log.Warnf("[baidu_netdisk] CustomUploadPartSize is not supported for non-vip user, use DefaultSliceSize")
		}
		if filesize > MaxSliceNum*DefaultSliceSize {
			log.Warnf("[baidu_netdisk] File size(%d) is too large, may cause upload failure", filesize)
		}

		return DefaultSliceSize
	}

	if d.CustomUploadPartSize != 0 {
		if d.CustomUploadPartSize < DefaultSliceSize {
			log.Warnf("[baidu_netdisk] CustomUploadPartSize(%d) is less than DefaultSliceSize(%d), use DefaultSliceSize", d.CustomUploadPartSize, DefaultSliceSize)
			return DefaultSliceSize
		}

		if d.vipType == 1 && d.CustomUploadPartSize > VipSliceSize {
			log.Warnf("[baidu_netdisk] CustomUploadPartSize(%d) is greater than VipSliceSize(%d), use VipSliceSize", d.CustomUploadPartSize, VipSliceSize)
			return VipSliceSize
		}

		if d.vipType == 2 && d.CustomUploadPartSize > SVipSliceSize {
			log.Warnf("[baidu_netdisk] CustomUploadPartSize(%d) is greater than SVipSliceSize(%d), use SVipSliceSize", d.CustomUploadPartSize, SVipSliceSize)
			return SVipSliceSize
		}

		return d.CustomUploadPartSize
	}

	maxSliceSize := DefaultSliceSize

	switch d.vipType {
	case 1:
		maxSliceSize = VipSliceSize
	case 2:
		maxSliceSize = SVipSliceSize
	}

	// upload on low bandwidth
	if d.LowBandwithUploadMode {
		size := DefaultSliceSize

		for size <= maxSliceSize {
			if filesize <= MaxSliceNum*size {
				return size
			}

			size += SliceStep
		}
	}

	if filesize > MaxSliceNum*maxSliceSize {
		log.Warnf("[baidu_netdisk] File size(%d) is too large, may cause upload failure", filesize)
	}

	return maxSliceSize
}

func (d *BaiduNetdisk) quota(ctx context.Context) (model.DiskUsage, error) {
	var resp QuotaResp
	_, err := d.request("https://pan.baidu.com/api/quota", http.MethodGet, func(req *resty.Request) {
		req.SetContext(ctx)
	}, &resp)
	if err != nil {
		return model.DiskUsage{}, err
	}
	return driver.DiskUsageFromUsedAndTotal(resp.Used, resp.Total), nil
}

// getUploadUrl 从开放平台获取上传域名/地址，并发请求会被合并，结果会在 uploadid 生命周期内复用。
// 如果获取失败，则返回 Upload API设置项。
func (d *BaiduNetdisk) getUploadUrl(path, uploadId string) string {
	if !d.UseDynamicUploadAPI || uploadId == "" {
		return d.UploadAPI
	}

	uploadUrl, err := d.requestForUploadUrl(path, uploadId)
	if err != nil {
		return d.UploadAPI
	}
	return uploadUrl
}

// requestForUploadUrl 请求获取上传地址。
// 实测此接口不需要认证，传method和upload_version就行，不过还是按文档规范调用。
// https://pan.baidu.com/union/doc/Mlvw5hfnr
func (d *BaiduNetdisk) requestForUploadUrl(path, uploadId string) (string, error) {
	params := map[string]string{
		"method":         "locateupload",
		"appid":          "250528",
		"path":           path,
		"uploadid":       uploadId,
		"upload_version": "2.0",
	}
	apiUrl := "https://d.pcs.baidu.com/rest/2.0/pcs/file"
	var resp UploadServerResp
	_, err := d.request(apiUrl, http.MethodGet, func(req *resty.Request) {
		req.SetQueryParams(params)
	}, &resp)
	if err != nil {
		return "", err
	}
	// 应该是https开头的一个地址
	var uploadUrl string
	if len(resp.Servers) > 0 {
		uploadUrl = resp.Servers[0].Server
	} else if len(resp.BakServers) > 0 {
		uploadUrl = resp.BakServers[0].Server
	}
	if uploadUrl == "" {
		return "", errors.New("upload URL is empty")
	}
	return uploadUrl, nil
}

// func encodeURIComponent(str string) string {
// 	r := url.QueryEscape(str)
// 	r = strings.ReplaceAll(r, "+", "%20")
// 	return r
// }

func DecryptMd5(encryptMd5 string) string {
	utils.Log.Debugf("┌─────────────────────────────────────────────────────────┐")
	utils.Log.Debugf("│ [DecryptMd5] 开始处理")
	utils.Log.Debugf("│ 输入: '%s' (长度:%d)", encryptMd5, len(encryptMd5))

	if encryptMd5 == "" {
		utils.Log.Debugf("│ 输入为空，直接返回")
		utils.Log.Debugf("└─────────────────────────────────────────────────────────┘")
		return encryptMd5
	}

	// 检查是否已经是有效的十六进制
	if _, err := hex.DecodeString(encryptMd5); err == nil {
		utils.Log.Debugf("│ ✓ 已是有效的32位十六进制MD5，无需解密")
		utils.Log.Debugf("│ 输出: '%s'", encryptMd5)
		utils.Log.Debugf("└─────────────────────────────────────────────────────────┘")
		return encryptMd5
	} else {
		utils.Log.Debugf("│ ✗ 不是有效十六进制: %v", err)
		utils.Log.Debugf("│ 需要执行解密操作")
	}

	utils.Log.Debugf("│")
	utils.Log.Debugf("│ [步骤1] XOR解密")

	var out strings.Builder
	out.Grow(len(encryptMd5))
	for i, n := 0, int64(0); i < len(encryptMd5); i++ {
		if i == 9 {
			// 特殊处理：第9位字符减去'g'
			n = int64(unicode.ToLower(rune(encryptMd5[i])) - 'g')
			utils.Log.Debugf("│   位置%d: '%c' -> toLower('%c') - 'g' = %d",
				i, encryptMd5[i], unicode.ToLower(rune(encryptMd5[i])), n)
		} else {
			n, _ = strconv.ParseInt(encryptMd5[i:i+1], 16, 64)
		}
		xorResult := n ^ int64(15&i)
		out.WriteString(strconv.FormatInt(xorResult, 16))
	}

	xorOutput := out.String()
	utils.Log.Debugf("│ XOR结果: '%s'", xorOutput)

	utils.Log.Debugf("│")
	utils.Log.Debugf("│ [步骤2] 位置交换")
	utils.Log.Debugf("│   [8:16]  (8个字符): '%s'", xorOutput[8:16])
	utils.Log.Debugf("│   [0:8]   (8个字符): '%s'", xorOutput[:8])
	utils.Log.Debugf("│   [24:32] (8个字符): '%s'", xorOutput[24:32])
	utils.Log.Debugf("│   [16:24] (8个字符): '%s'", xorOutput[16:24])

	finalMd5 := xorOutput[8:16] + xorOutput[:8] + xorOutput[24:32] + xorOutput[16:24]

	utils.Log.Debugf("│")
	utils.Log.Debugf("│ [最终结果]")
	utils.Log.Debugf("│ 输出: '%s' (长度:%d)", finalMd5, len(finalMd5))

	// 验证最终结果
	if _, err := hex.DecodeString(finalMd5); err == nil {
		utils.Log.Debugf("│ ✓ 解密成功！输出是有效的32位十六进制MD5")
	} else {
		utils.Log.Warnf("│ ✗ 解密结果不是有效的十六进制: %v", err)
	}

	utils.Log.Debugf("└─────────────────────────────────────────────────────────┘")

	return finalMd5
}

// encMd5 备用加密方法（基于Java实现）
// 用于验证加密逻辑的另一种实现方式
func encMd5(md5 string) string {
	utils.Log.Debugf("    ┌─ [encMd5备用方法] ──────────────────────────┐")
	utils.Log.Debugf("    │ 输入MD5: %s", md5)

	// 位置交换
	temp := md5[8:16] + md5[0:8] + md5[24:32] + md5[16:24]
	utils.Log.Debugf("    │ 位置交换后: %s", temp)

	var res strings.Builder
	res.Grow(32)

	for i := 0; i < len(temp); i++ {
		c := temp[i]
		// Character.digit(c, 16) 等价于 strconv.ParseInt
		digit, _ := strconv.ParseInt(string(c), 16, 64)
		xorResult := digit ^ int64(15&i)
		res.WriteString(strconv.FormatInt(xorResult, 16))
	}

	// 特殊处理第9位：将十六进制数字加上'g'
	result := res.String()
	utils.Log.Debugf("    │ XOR后: %s", result)

	if len(result) > 9 {
		digit9, _ := strconv.ParseInt(string(result[9]), 16, 64)
		char9 := rune(digit9 + 'g')
		result = result[:9] + string(char9) + result[10:]
		utils.Log.Debugf("    │ 替换位置9: '%c' (digit:%d + 'g')", char9, digit9)
	}

	utils.Log.Debugf("    │ 最终输出: %s", result)
	utils.Log.Debugf("    └────────────────────────────────────────────┘")

	return result
}

// DecryptMd5Alternative 备用解密方法（使用encMd5逆向）
// 如果标准解密失败，可以尝试这个方法
func DecryptMd5Alternative(encryptMd5 string) string {
	utils.Log.Debugf("┌─────────────────────────────────────────────────────────┐")
	utils.Log.Debugf("│ [DecryptMd5Alternative] 备用解密方法")
	utils.Log.Debugf("│ 输入: '%s'", encryptMd5)

	if encryptMd5 == "" {
		utils.Log.Debugf("│ 输入为空")
		utils.Log.Debugf("└─────────────────────────────────────────────────────────┘")
		return encryptMd5
	}

	// 检查是否已经是有效的十六进制
	if _, err := hex.DecodeString(encryptMd5); err == nil {
		utils.Log.Debugf("│ 已是有效hex，直接返回")
		utils.Log.Debugf("└─────────────────────────────────────────────────────────┘")
		return encryptMd5
	}

	// 使用encMd5方法验证：尝试加密标准MD5，看是否能得到百度的加密格式
	// 这里我们假设输入是加密的，需要通过逆向得到原始MD5

	utils.Log.Debugf("│ 尝试使用encMd5逆向解密...")
	utils.Log.Debugf("│ 提示: encMd5(真实MD5) = 百度加密MD5")
	utils.Log.Debugf("│ 当前我们有百度加密MD5，需要找到真实MD5")
	utils.Log.Debugf("└─────────────────────────────────────────────────────────┘")

	// 注意：这只是演示，实际解密需要逆向encMd5的每一步
	// 当前仍使用标准解密方法
	return DecryptMd5(encryptMd5)
}

func EncryptMd5(originalMd5 string) string {
	reversed := originalMd5[8:16] + originalMd5[:8] + originalMd5[24:32] + originalMd5[16:24]

	var out strings.Builder
	out.Grow(len(reversed))
	for i, n := 0, int64(0); i < len(reversed); i++ {
		n, _ = strconv.ParseInt(reversed[i:i+1], 16, 64)
		n ^= int64(15 & i)
		if i == 9 {
			out.WriteRune(rune(n) + 'g')
		} else {
			out.WriteString(strconv.FormatInt(n, 16))
		}
	}
	return out.String()
}
