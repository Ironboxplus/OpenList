package _115_open

import (
	"errors"
	"testing"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
)

// driver.Get's only job after Fix 4 is "is this a folder, and if so what's
// its FileID/name". Files are rejected with errs.NotImplement so op.Get
// falls back to the list-based path — that response carries
// GetFilesResp_File.FS as int64 and is always correct. The 115
// "Get folder info" API (yuque rl8zrhe2nag21dfw) is folder-oriented and
// resp.Size is unreliable for file paths anyway.

func TestFolderInfoToObj_Folder(t *testing.T) {
	resp := &sdk.GetFolderInfoResp{
		FileID: "1234", FileName: "Movies", FileCategory: "0",
	}
	obj := folderInfoToObj(resp)
	if !obj.IsDir() {
		t.Fatalf("IsDir() = false, want true (FileCategory 0 = folder)")
	}
	if obj.GetID() != "1234" || obj.GetName() != "Movies" {
		t.Fatalf("obj = %+v, want id=1234 name=Movies", obj)
	}
}

func TestFromFolderInfo_FileReturnsNotImplement(t *testing.T) {
	// File path response: even with sha1/pickcode populated, fast-path
	// must defer to list — Size is not byte-accurate for files.
	resp := &sdk.GetFolderInfoResp{
		FileID: "3370024725891437210", FileName: "Django Unchained 2012.mkv",
		FileCategory: "1", Sha1: "17C6301550DE8E22D477EB9BA3901A99B9961494",
		PickCode: "cqghjg71avvncddhi", Size: "36767958354",
	}
	obj, err := fromFolderInfo(resp)
	if obj != nil {
		t.Fatalf("obj = %+v, want nil (files must defer to List)", obj)
	}
	if !errors.Is(err, errs.NotImplement) {
		t.Fatalf("err = %v, want errs.NotImplement (so op.Get falls through to list path)", err)
	}
}

func TestFromFolderInfo_FolderReturnsObj(t *testing.T) {
	resp := &sdk.GetFolderInfoResp{
		FileID: "1234", FileName: "Movies", FileCategory: "0",
	}
	obj, err := fromFolderInfo(resp)
	if err != nil {
		t.Fatalf("unexpected err = %v", err)
	}
	if obj == nil || !obj.IsDir() || obj.GetID() != "1234" {
		t.Fatalf("obj = %+v, want non-nil folder with ID 1234", obj)
	}
}
