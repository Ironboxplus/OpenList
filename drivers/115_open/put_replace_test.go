package _115_open

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// folder115 answers upload/init and ufile/delete for one folder the way the
// 115 Open API did when measured on 2026-11-13 (tmp/dup115-probe.py,
// tmp/dup115-instant-probe.py against the real API):
//   - an upload whose name the folder already has adds a second file of that
//     name; the folder then holds both;
//   - an instant upload of content 115 already has answers status 2 with the
//     file_id of the file it made, and under a name the folder already holds
//     with that very content it makes nothing and answers with that file's id.
type folder115 struct {
	mu    sync.Mutex
	files []*Obj
	next  int
}

func (f *folder115) handle(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/open/upload/init":
			name, sha := r.FormValue("file_name"), r.FormValue("fileid")
			for _, o := range f.files {
				if o.Fn == name && strings.EqualFold(o.Sha1, sha) {
					writeSDKSuccess(t, w, map[string]any{"status": 2, "file_id": o.Fid, "pick_code": "pc-" + o.Fid})
					return
				}
			}
			f.next++
			o := &Obj{Fid: fmt.Sprintf("new-%d", f.next), Pid: "dir-1", Fn: name, Sha1: sha, Fc: "1"}
			f.files = append(f.files, o)
			writeSDKSuccess(t, w, map[string]any{"status": 2, "file_id": o.Fid, "pick_code": "pc-" + o.Fid})
		case "/open/ufile/delete":
			ids := strings.Split(r.FormValue("file_ids"), ",")
			f.files = slices.DeleteFunc(f.files, func(o *Obj) bool { return slices.Contains(ids, o.Fid) })
			writeSDKSuccess(t, w, []string{})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}
}

func (f *folder115) named(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, o := range f.files {
		if o.Fn == name {
			ids = append(ids, o.Fid)
		}
	}
	return ids
}

// Whole-file SHA1s: only a full-width hash takes the instant-upload path.
var (
	shaA = strings.Repeat("A", 40)
	shaB = strings.Repeat("B", 40)
)

// existingStreamer is an upload over a file that is already there, as op.Put
// hands it to the driver.
type existingStreamer struct {
	mockFileStreamer
	exist model.Obj
}

func (s *existingStreamer) GetExist() model.Obj { return s.exist }

func uploadOver(name, sha string, exist model.Obj) *existingStreamer {
	return &existingStreamer{
		mockFileStreamer: mockFileStreamer{
			name: name,
			size: 100,
			hashInfo: utils.NewHashInfoByMap(map[*utils.HashType]string{
				utils.SHA1:      sha,
				utils.SHA1_128K: sha,
			}),
			data: make([]byte, 100),
		},
		exist: exist,
	}
}

// An upload over a file replaces it: the folder is left with one file of that
// name, the new one, and not with the old one beside it.
func TestPutOverAFileReplacesIt(t *testing.T) {
	old := &Obj{Fid: "old-1", Pid: "dir-1", Fn: "f.txt", Sha1: shaA, Fc: "1"}
	folder := &folder115{files: []*Obj{old}}
	d, _ := newTestOpen115(t, "trash", folder.handle(t))
	dir := &model.Object{ID: "dir-1", Name: "dir", IsFolder: true}

	if err := d.Put(context.Background(), dir, uploadOver("f.txt", shaB, old), func(float64) {}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got := folder.named("f.txt")
	if len(got) != 1 || got[0] == "old-1" {
		t.Fatalf("files named f.txt after the upload over it: %v, want only the new one", got)
	}
}

// Uploading the very content a file already has under its name makes no new
// file (115 answers with the existing id): the file stays.
func TestPutOfTheSameContentKeepsTheFile(t *testing.T) {
	old := &Obj{Fid: "old-1", Pid: "dir-1", Fn: "f.txt", Sha1: shaA, Fc: "1"}
	folder := &folder115{files: []*Obj{old}}
	d, requests := newTestOpen115(t, "trash", folder.handle(t))
	dir := &model.Object{ID: "dir-1", Name: "dir", IsFolder: true}

	if err := d.Put(context.Background(), dir, uploadOver("f.txt", shaA, old), func(float64) {}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := folder.named("f.txt"); len(got) != 1 || got[0] != "old-1" {
		t.Fatalf("files named f.txt: %v, want the one that was there", got)
	}
	for _, r := range requests() {
		if r.Path == "/open/ufile/delete" {
			t.Fatalf("removed a file for an upload that made none: %v", r.Form)
		}
	}
}
