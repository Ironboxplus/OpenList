package baidu_youth

import (
	"errors"
	"path"
	"strconv"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

var (
	ErrBaiduYouthEmptyFilesNotAllowed = errors.New("empty files are not allowed by baidu youth")
)

type File struct {
	Category int   `json:"category"`
	FsId     int64 `json:"fs_id"`
	Thumbs   struct {
		Url3 string `json:"url3"`
	} `json:"thumbs"`
	Size           int64  `json:"size"`
	Path           string `json:"path"`
	ServerFilename string `json:"server_filename"`
	Md5            string `json:"md5"`
	Isdir          int    `json:"isdir"`
	ServerCtime    int64  `json:"server_ctime"`
	ServerMtime    int64  `json:"server_mtime"`
	LocalMtime     int64  `json:"local_mtime"`
	LocalCtime     int64  `json:"local_ctime"`
	Ctime          int64  `json:"ctime"`
	Mtime          int64  `json:"mtime"`
	Dlink          string `json:"dlink"`
}

func fileToObj(f File) *model.ObjThumb {
	if f.ServerFilename == "" {
		f.ServerFilename = path.Base(f.Path)
	}
	if f.ServerCtime == 0 {
		if f.LocalCtime != 0 {
			f.ServerCtime = f.LocalCtime
		} else {
			f.ServerCtime = f.Ctime
		}
	}
	if f.ServerMtime == 0 {
		if f.LocalMtime != 0 {
			f.ServerMtime = f.LocalMtime
		} else {
			f.ServerMtime = f.Mtime
		}
	}
	return &model.ObjThumb{
		Object: model.Object{
			ID:       strconv.FormatInt(f.FsId, 10),
			Path:     f.Path,
			Name:     f.ServerFilename,
			Size:     f.Size,
			Modified: time.Unix(f.ServerMtime, 0),
			Ctime:    time.Unix(f.ServerCtime, 0),
			IsFolder: f.Isdir == 1,
			HashInfo: utils.NewHashInfo(utils.MD5, DecryptMd5(f.Md5)),
		},
		Thumbnail: model.Thumbnail{Thumbnail: f.Thumbs.Url3},
	}
}

type ListResp struct {
	Errno int    `json:"errno"`
	List  []File `json:"list"`
}

type FileMetaResp struct {
	Errno  int    `json:"errno"`
	Errmsg string `json:"errmsg"`
	Info   []File `json:"info"`
	List   []File `json:"list"`
}

type LocateDownloadResp struct {
	Errno   int    `json:"errno"`
	Errmsg  string `json:"errmsg"`
	ShowMsg string `json:"show_msg"`
	Path    string `json:"path"`
	URL     string `json:"url"`
}

type MediaInfoResp struct {
	Errno   int    `json:"errno"`
	Errmsg  string `json:"errmsg"`
	ShowMsg string `json:"show_msg"`
	Info    File   `json:"info"`
}

type CreateResp struct {
	Errno   int    `json:"errno"`
	Errmsg  string `json:"errmsg"`
	ShowMsg string `json:"show_msg"`
	Info    File   `json:"info"`
	File
}

func (r CreateResp) ResultFile() File {
	if r.Info.Path != "" || r.Info.FsId != 0 {
		return r.Info
	}
	return r.File
}

type PrecreateResp struct {
	Errno      int    `json:"errno"`
	Errmsg     string `json:"errmsg"`
	ShowMsg    string `json:"show_msg"`
	ReturnType int    `json:"return_type"`
	Path       string `json:"path"`
	Uploadid   string `json:"uploadid"`
	Uploadsign string `json:"uploadsign"`
	BlockList  []int  `json:"block_list"`
	Info       File   `json:"info"`
	File
}

func (r PrecreateResp) ResultFile() File {
	if r.Info.Path != "" || r.Info.FsId != 0 {
		return r.Info
	}
	return r.File
}
