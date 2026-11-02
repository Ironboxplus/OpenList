package darkibox

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Darkibox struct {
	model.Storage
	Addition
}

func (d *Darkibox) Config() driver.Config {
	return config
}

func (d *Darkibox) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Darkibox) Init(ctx context.Context) error {
	if d.APIKey == "" {
		return fmt.Errorf("API key is required")
	}
	if d.RootFolderID == "" {
		d.RootFolderID = "0"
	}

	// Verify API key by calling account/info
	var account accountInfoResult
	if err := d.callAPI(ctx, "/account/info", nil, &account); err != nil {
		return fmt.Errorf("failed to verify API key: %w", err)
	}

	op.MustSaveDriverStorage(d)
	return nil
}

func (d *Darkibox) Drop(ctx context.Context) error {
	return nil
}

func (d *Darkibox) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	folderID := d.RootFolderID
	if dir.GetID() != "" {
		folderID = folderIDFromObjID(dir.GetID())
	}

	var objects []model.Obj

	// List sub-folders via /api/folder/list
	var folders folderListResult
	if err := d.callAPI(ctx, "/folder/list", map[string]string{
		"fld_id": fldIDStr(folderID),
	}, &folders); err != nil {
		return nil, fmt.Errorf("list folders failed: %w", err)
	}
	for _, f := range folders.Folders {
		objects = append(objects, &model.Object{
			ID:       encodeFolderID(f.FldID),
			Name:     f.Name,
			IsFolder: true,
		})
	}

	// List files via /api/file/list (paginated)
	page := 1
	for {
		var files fileListResult
		if err := d.callAPI(ctx, "/file/list", map[string]string{
			"fld_id":   fldIDStr(folderID),
			"per_page": "200",
			"page":     strconv.Itoa(page),
		}, &files); err != nil {
			return nil, fmt.Errorf("list files failed: %w", err)
		}

		for _, f := range files.Files {
			modified := time.Now()
			if f.Uploaded != "" {
				if t, err := time.Parse("2006-01-02 15:04:05", f.Uploaded); err == nil {
					modified = t
				}
			}
			name := f.Name
			if name == "" {
				name = f.Title
			}
			objects = append(objects, &model.Object{
				ID:       encodeFileID(f.FileCode),
				Name:     name,
				Size:     f.Size,
				Modified: modified,
				IsFolder: false,
			})
		}

		// Check if there are more pages
		if len(files.Files) < 200 {
			break
		}
		page++
	}

	return objects, nil
}

func (d *Darkibox) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if file.IsDir() {
		return nil, errs.NotFile
	}

	fileCode := fileCodeFromObjID(file.GetID())
	if fileCode == "" {
		return nil, fmt.Errorf("empty file code")
	}

	var result directLinkResult
	if err := d.callAPI(ctx, "/file/direct_link", map[string]string{
		"file_code": fileCode,
	}, &result); err != nil {
		return nil, fmt.Errorf("failed to get direct link: %w", err)
	}

	// Find the original quality version, fall back to first available
	var dlURL string
	for _, v := range result.Versions {
		if v.Name == "o" {
			dlURL = v.URL
			break
		}
	}
	if dlURL == "" && len(result.Versions) > 0 {
		dlURL = result.Versions[0].URL
	}
	if dlURL == "" {
		return nil, fmt.Errorf("no download URL available for file %s", fileCode)
	}

	return &model.Link{
		URL: dlURL,
	}, nil
}

func (d *Darkibox) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	parentID := d.RootFolderID
	if parentDir.GetID() != "" {
		parentID = folderIDFromObjID(parentDir.GetID())
	}

	var result folderCreateResult
	if err := d.callAPI(ctx, "/folder/create", map[string]string{
		"name":      dirName,
		"parent_id": fldIDStr(parentID),
	}, &result); err != nil {
		return nil, fmt.Errorf("create folder failed: %w", err)
	}

	return &model.Object{
		ID:       encodeFolderID(result.FldID),
		Name:     dirName,
		IsFolder: true,
	}, nil
}

func (d *Darkibox) Move(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	if srcObj.IsDir() {
		return nil, errs.NotImplement
	}

	fileCode := fileCodeFromObjID(srcObj.GetID())
	if fileCode == "" {
		return nil, fmt.Errorf("empty file code")
	}

	dstFolderID := d.RootFolderID
	if dstDir.GetID() != "" {
		dstFolderID = folderIDFromObjID(dstDir.GetID())
	}

	if err := d.callAPI(ctx, "/file/move", map[string]string{
		"file_code": fileCode,
		"to_folder": fldIDStr(dstFolderID),
	}, nil); err != nil {
		return nil, fmt.Errorf("move file failed: %w", err)
	}

	return &model.Object{
		ID:       srcObj.GetID(),
		Name:     srcObj.GetName(),
		Size:     srcObj.GetSize(),
		Modified: srcObj.ModTime(),
		IsFolder: false,
	}, nil
}

func (d *Darkibox) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	return nil, errs.NotImplement
}

func (d *Darkibox) Copy(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	return nil, errs.NotImplement
}

func (d *Darkibox) Remove(ctx context.Context, obj model.Obj) error {
	if obj.IsDir() {
		folderID := folderIDFromObjID(obj.GetID())
		return d.callAPI(ctx, "/folder/delete", map[string]string{
			"fld_id": folderID,
		}, nil)
	}

	fileCode := fileCodeFromObjID(obj.GetID())
	return d.callAPI(ctx, "/file/delete", map[string]string{
		"file_code": fileCode,
	}, nil)
}

func (d *Darkibox) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	folderID := d.RootFolderID
	if dstDir.GetID() != "" {
		folderID = folderIDFromObjID(dstDir.GetID())
	}

	// Step 1: Get the upload server URL
	var server uploadServerResult
	if err := d.callAPI(ctx, "/upload/server", nil, &server); err != nil {
		return nil, fmt.Errorf("get upload server failed: %w", err)
	}
	if server.URL == "" {
		return nil, fmt.Errorf("no upload server URL returned")
	}

	// Step 2: Upload the file to the upload server
	reader := driver.NewLimitedUploadStream(ctx, &driver.ReaderUpdatingProgress{
		Reader:         file,
		UpdateProgress: up,
	})

	res, err := base.RestyClient.R().
		SetContext(ctx).
		SetMultipartField("file", file.GetName(), "", reader).
		SetMultipartFormData(map[string]string{
			"key":    d.APIKey,
			"fld_id": fldIDStr(folderID),
		}).
		Post(server.URL)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	if res.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("upload failed: http %d", res.StatusCode())
	}

	// Try to parse upload response to get the file code
	var uploadResp uploadResult
	if err := base.RestyClient.JSONUnmarshal(res.Body(), &uploadResp); err == nil && len(uploadResp.Files) > 0 {
		uf := uploadResp.Files[0]
		return &model.Object{
			ID:       encodeFileID(uf.FileCode),
			Name:     file.GetName(),
			Size:     file.GetSize(),
			IsFolder: false,
		}, nil
	}

	return &model.Object{
		Name:     file.GetName(),
		Size:     file.GetSize(),
		IsFolder: false,
	}, nil
}

func (d *Darkibox) GetArchiveMeta(ctx context.Context, obj model.Obj, args model.ArchiveArgs) (model.ArchiveMeta, error) {
	return nil, errs.NotImplement
}

func (d *Darkibox) ListArchive(ctx context.Context, obj model.Obj, args model.ArchiveInnerArgs) ([]model.Obj, error) {
	return nil, errs.NotImplement
}

func (d *Darkibox) Extract(ctx context.Context, obj model.Obj, args model.ArchiveInnerArgs) (*model.Link, error) {
	return nil, errs.NotImplement
}

func (d *Darkibox) ArchiveDecompress(ctx context.Context, srcObj, dstDir model.Obj, args model.ArchiveDecompressArgs) ([]model.Obj, error) {
	return nil, errs.NotImplement
}

var _ driver.Driver = (*Darkibox)(nil)
