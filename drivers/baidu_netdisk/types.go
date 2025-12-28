package baidu_netdisk

import (
	"errors"
	"path"
	"strconv"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

var (
	ErrBaiduEmptyFilesNotAllowed = errors.New("empty files are not allowed by baidu netdisk")
)

type TokenErrResp struct {
	ErrorDescription string `json:"error_description"`
	Error            string `json:"error"`
}

type File struct {
	//TkbindId     int    `json:"tkbind_id"`
	//OwnerType    int    `json:"owner_type"`
	Category int `json:"category"`
	//RealCategory string `json:"real_category"`
	FsId int64 `json:"fs_id"`
	//OperId      int   `json:"oper_id"`
	Thumbs struct {
		//Icon string `json:"icon"`
		Url3 string `json:"url3"`
		//Url2 string `json:"url2"`
		//Url1 string `json:"url1"`
	} `json:"thumbs"`
	//Wpfile         int    `json:"wpfile"`

	Size int64 `json:"size"`
	//ExtentTinyint7 int    `json:"extent_tinyint7"`
	Path string `json:"path"`
	//Share          int    `json:"share"`
	//Pl             int    `json:"pl"`
	ServerFilename string `json:"server_filename"`
	Md5            string `json:"md5"`
	//OwnerId        int    `json:"owner_id"`
	//Unlist int `json:"unlist"`
	Isdir int `json:"isdir"`

	// list resp
	ServerCtime int64 `json:"server_ctime"`
	ServerMtime int64 `json:"server_mtime"`
	LocalMtime  int64 `json:"local_mtime"`
	LocalCtime  int64 `json:"local_ctime"`
	//ServerAtime    int64    `json:"server_atime"` `

	// only create and precreate resp
	Ctime int64 `json:"ctime"`
	Mtime int64 `json:"mtime"`
}

func fileToObj(f File) *model.ObjThumb {
	if f.ServerFilename == "" {
		f.ServerFilename = path.Base(f.Path)
	}
	if f.ServerCtime == 0 {
		f.ServerCtime = f.Ctime
	}
	if f.ServerMtime == 0 {
		f.ServerMtime = f.Mtime
	}

	// ========== DEBUG: MD5处理流程 ==========
	utils.Log.Debugf("╔══════════════════════════════════════════════════════════╗")
	utils.Log.Debugf("║ [百度网盘] 文件对象转换开始")
	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
	utils.Log.Debugf("║ 文件名: %s", f.ServerFilename)
	utils.Log.Debugf("║ 文件ID: %d", f.FsId)
	utils.Log.Debugf("║ 文件大小: %d bytes", f.Size)
	utils.Log.Debugf("║ 是否目录: %v", f.Isdir == 1)
	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")

	// 记录百度API返回的原始MD5
	originalMd5 := f.Md5
	utils.Log.Debugf("║ [步骤1] 百度API返回的原始MD5")
	utils.Log.Debugf("║   值: '%s'", originalMd5)
	utils.Log.Debugf("║   长度: %d", len(originalMd5))
	utils.Log.Debugf("║   是否为空: %v", originalMd5 == "")

	// 打印MD5的字节表示
	if originalMd5 != "" {
		utils.Log.Debugf("║   字节值: %v", []byte(originalMd5))
		// 检查是否包含非十六进制字符
		hasNonHex := false
		for i, ch := range originalMd5 {
			if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
				hasNonHex = true
				utils.Log.Warnf("║   ⚠ 位置%d包含非十六进制字符: '%c' (ASCII:%d)", i, ch, ch)
			}
		}
		if !hasNonHex {
			utils.Log.Debugf("║   ✓ 所有字符都是有效的十六进制字符")
		}
	}

	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
	utils.Log.Debugf("║ [步骤2] 调用DecryptMd5进行处理")
	utils.Log.Debugf("║   注释说明: '直接获取的MD5是错误的'")
	utils.Log.Debugf("║   问题: 百度返回的MD5格式如 '1261d72d03471f7b7b805fd60e024b8d'")
	utils.Log.Debugf("║   问题: 这已经是标准的32位十六进制，为何说是错误的？")

	// 执行解密
	decryptedMd5 := DecryptMd5(f.Md5)

	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
	utils.Log.Debugf("║ [步骤3] DecryptMd5处理结果")
	utils.Log.Debugf("║   原始MD5: '%s'", originalMd5)
	utils.Log.Debugf("║   处理后MD5: '%s'", decryptedMd5)
	utils.Log.Debugf("║   是否相同: %v", originalMd5 == decryptedMd5)

	if originalMd5 != decryptedMd5 {
		utils.Log.Warnf("║   ⚠ MD5被修改了！")
		utils.Log.Warnf("║   这意味着执行了解密/转换操作")

		// 测试备用加密方法：使用解密后的MD5加密，看是否能还原成百度的格式
		utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
		utils.Log.Debugf("║ [验证] 使用encMd5测试逆向过程")
		utils.Log.Debugf("║   假设: encMd5(解密后MD5) 应该 = 百度加密MD5")
		reEncrypted := encMd5(decryptedMd5)
		utils.Log.Debugf("║   encMd5('%s') = '%s'", decryptedMd5, reEncrypted)
		utils.Log.Debugf("║   是否匹配百度原始: %v", reEncrypted == originalMd5)
		if reEncrypted == originalMd5 {
			utils.Log.Debugf("║   ✓ 完美匹配！解密算法正确")
		} else {
			utils.Log.Warnf("║   ✗ 不匹配！")
			utils.Log.Warnf("║   百度原始: %s", originalMd5)
			utils.Log.Warnf("║   重新加密: %s", reEncrypted)
		}
	} else {
		utils.Log.Debugf("║   ✓ MD5未被修改（直接返回原值）")
		utils.Log.Debugf("║   这意味着百度返回的就是标准格式")
	}

	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
	utils.Log.Debugf("║ [步骤4] 创建HashInfo对象")

	// 创建HashInfo
	hashInfo := utils.NewHashInfo(utils.MD5, decryptedMd5)

	utils.Log.Debugf("║   已将MD5传入HashInfo")
	utils.Log.Debugf("╠══════════════════════════════════════════════════════════╣")
	utils.Log.Debugf("║ [关键问题分析]")
	utils.Log.Debugf("║ 1. 百度API现在返回的MD5格式是什么？")
	utils.Log.Debugf("║    - 如果是标准32位hex: DecryptMd5不应该做任何处理")
	utils.Log.Debugf("║    - 如果是加密格式: DecryptMd5需要解密")
	utils.Log.Debugf("║ 2. '直接获取的MD5是错误的'这个注释是否过时？")
	utils.Log.Debugf("║    - 可能百度以前返回加密MD5，现在已经改为标准格式")
	utils.Log.Debugf("║ 3. 解密后的MD5是否是文件的真实MD5？")
	utils.Log.Debugf("║    - 这是核心问题！")
	utils.Log.Debugf("║    - 如果解密后MD5 ≠ 文件真实MD5：")
	utils.Log.Debugf("║      → 上传到123网盘时MD5校验会失败")
	utils.Log.Debugf("║      → 必须重新计算文件真实MD5")
	utils.Log.Debugf("║    - 如果解密后MD5 = 文件真实MD5：")
	utils.Log.Debugf("║      → 可以直接使用，秒传成功")
	utils.Log.Debugf("║ 4. 百度的MD5加密/解密可能只是防爬虫手段")
	utils.Log.Debugf("║    - 解密后的MD5可能仍然不是文件真实MD5")
	utils.Log.Debugf("║    - 只是百度内部的文件标识符")
	utils.Log.Debugf("║ 5. 如果Etag验证失败，可能的原因：")
	utils.Log.Debugf("║    - MD5本身格式正确，但不是文件真实MD5")
	utils.Log.Debugf("║    - 客户端对Etag格式有特殊要求")
	utils.Log.Debugf("║    - Etag的引号、大小写等格式问题")
	utils.Log.Debugf("║")
	utils.Log.Debugf("║ [建议]")
	utils.Log.Debugf("║ 从百度网盘复制到其他网盘时:")
	utils.Log.Debugf("║ - 不要信任百度提供的MD5（无论是否解密）")
	utils.Log.Debugf("║ - 强制重新计算文件的真实MD5")
	utils.Log.Debugf("║ - 或者在目标网盘驱动中添加MD5验证逻辑")
	utils.Log.Debugf("╚══════════════════════════════════════════════════════════╝")

	return &model.ObjThumb{
		Object: model.Object{
			ID:       strconv.FormatInt(f.FsId, 10),
			Path:     f.Path,
			Name:     f.ServerFilename,
			Size:     f.Size,
			Modified: time.Unix(f.ServerMtime, 0),
			Ctime:    time.Unix(f.ServerCtime, 0),
			IsFolder: f.Isdir == 1,

			// 直接获取的MD5是错误的
			HashInfo: hashInfo,
		},
		Thumbnail: model.Thumbnail{Thumbnail: f.Thumbs.Url3},
	}
}

type ListResp struct {
	Errno     int    `json:"errno"`
	GuidInfo  string `json:"guid_info"`
	List      []File `json:"list"`
	RequestId int64  `json:"request_id"`
	Guid      int    `json:"guid"`
}

type DownloadResp struct {
	Errmsg string `json:"errmsg"`
	Errno  int    `json:"errno"`
	List   []struct {
		//Category    int    `json:"category"`
		//DateTaken   int    `json:"date_taken,omitempty"`
		Dlink string `json:"dlink"`
		//Filename    string `json:"filename"`
		//FsId        int64  `json:"fs_id"`
		//Height      int    `json:"height,omitempty"`
		//Isdir       int    `json:"isdir"`
		//Md5         string `json:"md5"`
		//OperId      int    `json:"oper_id"`
		//Path        string `json:"path"`
		//ServerCtime int    `json:"server_ctime"`
		//ServerMtime int    `json:"server_mtime"`
		//Size        int    `json:"size"`
		//Thumbs      struct {
		//	Icon string `json:"icon,omitempty"`
		//	Url1 string `json:"url1,omitempty"`
		//	Url2 string `json:"url2,omitempty"`
		//	Url3 string `json:"url3,omitempty"`
		//} `json:"thumbs"`
		//Width int `json:"width,omitempty"`
	} `json:"list"`
	//Names struct {
	//} `json:"names"`
	RequestId string `json:"request_id"`
}

type DownloadResp2 struct {
	Errno int `json:"errno"`
	Info  []struct {
		//ExtentTinyint4 int `json:"extent_tinyint4"`
		//ExtentTinyint1 int `json:"extent_tinyint1"`
		//Bitmap string `json:"bitmap"`
		//Category int `json:"category"`
		//Isdir int `json:"isdir"`
		//Videotag int `json:"videotag"`
		Dlink string `json:"dlink"`
		//OperID int64 `json:"oper_id"`
		//PathMd5 int `json:"path_md5"`
		//Wpfile int `json:"wpfile"`
		//LocalMtime int `json:"local_mtime"`
		/*Thumbs struct {
			Icon string `json:"icon"`
			URL3 string `json:"url3"`
			URL2 string `json:"url2"`
			URL1 string `json:"url1"`
		} `json:"thumbs"`*/
		//PlaySource int `json:"play_source"`
		//Share int `json:"share"`
		//FileKey string `json:"file_key"`
		//Errno int `json:"errno"`
		//LocalCtime int `json:"local_ctime"`
		//Rotate int `json:"rotate"`
		//Metadata time.Time `json:"metadata"`
		//Height int `json:"height"`
		//SampleRate int `json:"sample_rate"`
		//Width int `json:"width"`
		//OwnerType int `json:"owner_type"`
		//Privacy int `json:"privacy"`
		//ExtentInt3 int64 `json:"extent_int3"`
		//RealCategory string `json:"real_category"`
		//SrcLocation string `json:"src_location"`
		//MetaInfo string `json:"meta_info"`
		//ID string `json:"id"`
		//Duration int `json:"duration"`
		//FileSize string `json:"file_size"`
		//Channels int `json:"channels"`
		//UseSegment int `json:"use_segment"`
		//ServerCtime int `json:"server_ctime"`
		//Resolution string `json:"resolution"`
		//OwnerID int `json:"owner_id"`
		//ExtraInfo string `json:"extra_info"`
		//Size int `json:"size"`
		//FsID int64 `json:"fs_id"`
		//ExtentTinyint3 int `json:"extent_tinyint3"`
		//Md5 string `json:"md5"`
		//Path string `json:"path"`
		//FrameRate int `json:"frame_rate"`
		//ExtentTinyint2 int `json:"extent_tinyint2"`
		//ServerFilename string `json:"server_filename"`
		//ServerMtime int `json:"server_mtime"`
		//TkbindID int `json:"tkbind_id"`
	} `json:"info"`
	RequestID int64 `json:"request_id"`
}

type PrecreateResp struct {
	Errno      int   `json:"errno"`
	RequestId  int64 `json:"request_id"`
	ReturnType int   `json:"return_type"`

	// return_type=1
	Path      string `json:"path"`
	Uploadid  string `json:"uploadid"`
	BlockList []int  `json:"block_list"`

	// return_type=2
	File File `json:"info"`

	UploadURL string `json:"-"` // 保存断点续传对应的上传域名
}

type UploadServerResp struct {
	BakServer  []any `json:"bak_server"`
	BakServers []struct {
		Server string `json:"server"`
	} `json:"bak_servers"`
	ClientIP    string `json:"client_ip"`
	ErrorCode   int    `json:"error_code"`
	ErrorMsg    string `json:"error_msg"`
	Expire      int    `json:"expire"`
	Host        string `json:"host"`
	Newno       string `json:"newno"`
	QuicServer  []any  `json:"quic_server"`
	QuicServers []struct {
		Server string `json:"server"`
	} `json:"quic_servers"`
	RequestID  int64 `json:"request_id"`
	Server     []any `json:"server"`
	ServerTime int   `json:"server_time"`
	Servers    []struct {
		Server string `json:"server"`
	} `json:"servers"`
	Sl int `json:"sl"`
}

type QuotaResp struct {
	Errno     int    `json:"errno"`
	RequestId int64  `json:"request_id"`
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	//Free      uint64 `json:"free"`
	//Expire    bool   `json:"expire"`
}
