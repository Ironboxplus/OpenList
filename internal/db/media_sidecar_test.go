package db

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestMediaSidecarsReplaceListAndDeleteWithItem(t *testing.T) {
	newCurationDB(t)
	if err := db.AutoMigrate(&model.MediaAnnotation{}); err != nil {
		t.Fatal(err)
	}
	a, b := scanned("A"), scanned("B")
	for _, it := range []*model.MediaItem{a, b} {
		if err := CreateOrUpdateMediaItem(it); err != nil {
			t.Fatal(err)
		}
	}
	if err := ReplaceMediaSidecars(a.ID, []model.MediaSidecar{
		{Video: "x.mkv", Path: "x.chs.ass"}, {Video: "x.mkv", Path: "x.srt"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceMediaSidecars(b.ID, []model.MediaSidecar{{Video: "y.mkv", Path: "y.srt"}}); err != nil {
		t.Fatal(err)
	}
	// a rescan replaces the list of one item and leaves the other
	if err := ReplaceMediaSidecars(a.ID, []model.MediaSidecar{{Video: "x.mkv", Path: "x.eng.srt"}}); err != nil {
		t.Fatal(err)
	}
	got, err := ListMediaSidecars([]uint{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[a.ID]) != 1 || got[a.ID][0].Path != "x.eng.srt" || len(got[b.ID]) != 1 {
		t.Fatalf("got %+v", got)
	}
	if err := DeleteMediaItem(a.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = ListMediaSidecars([]uint{a.ID, b.ID})
	if len(got[a.ID]) != 0 || len(got[b.ID]) != 1 {
		t.Fatalf("after delete: %+v", got)
	}
	if err := ReplaceMediaSidecars(b.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = ListMediaSidecars([]uint{b.ID}); len(got[b.ID]) != 0 {
		t.Fatalf("after clear: %+v", got)
	}
}
