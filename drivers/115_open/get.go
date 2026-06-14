package _115_open

import (
	"errors"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// isObjectNotFound reports whether err means the 115 API could not find the
// requested path, so driver.Get can translate it into errs.ObjectNotFound and
// let op.Get fall through instead of surfacing a raw SDK error. The SDK
// surfaces "not found" two ways and both must be honored:
//   - ErrDataEmpty: GetFolderInfoByPath returned an empty array. This is the
//     realistic case — request.go collapses an empty/`[]` payload to
//     ErrDataEmpty before the folder-info unmarshal can run.
//   - ErrObjectNotFound: upstream #2596's mapping of SDK not-found errors.
//     Currently shadowed for this call path but kept as a forward-compatible
//     second arm so a future SDK that surfaces it directly still works.
func isObjectNotFound(err error) bool {
	return errors.Is(err, sdk.ErrObjectNotFound) || errors.Is(err, sdk.ErrDataEmpty)
}

// folderInfoToObj extracts the fields driver.Get needs from a
// GetFolderInfoByPath response. We only look at FileID + FileName +
// FileCategory — Sha1 / PickCode come along because the struct already
// carries them, but Size is intentionally dropped: the API is
// folder-oriented and resp.Size is empty/garbage for file paths.
// fromFolderInfo rejects files outright so the parsed value would never
// be read anyway.
func folderInfoToObj(resp *sdk.GetFolderInfoResp) *Obj {
	return &Obj{
		Fid:  resp.FileID,
		Fn:   resp.FileName,
		Fc:   resp.FileCategory,
		Sha1: resp.Sha1,
		Pc:   resp.PickCode,
	}
}

// fromFolderInfo is the gateway used by driver.Get. Files are rejected
// with errs.NotImplement so op.Get falls through to its list-based
// path — that response carries GetFilesResp_File.FS (int64) and is
// always correct. Folders take the fast path: one
// GetFolderInfoByPath call is enough to build the Obj that
// op.list will pass to driver.List as the parent directory.
//
// Trade-off: cold-cache file access pays one wasted
// GetFolderInfoByPath (this call) + one folder GetFolderInfoByPath
// (parent) + one GetFiles (list parent) + the eventual DownURL = 4
// WaitLimit-gated SDK calls. Default limit_rate is 1 req/s so this
// is ~3s of pure rate-limit wait on top of network. Steady-state
// access within dirCache TTL (5 min) collapses back to a single
// DownURL call.
func fromFolderInfo(resp *sdk.GetFolderInfoResp) (model.Obj, error) {
	obj := folderInfoToObj(resp)
	if !obj.IsDir() {
		return nil, errs.NotImplement
	}
	return obj, nil
}
