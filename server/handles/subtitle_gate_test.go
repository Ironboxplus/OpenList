package handles

import (
	"context"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/stream"
)

type limitedStorage struct{ n int }

func (l limitedStorage) MaxDownloadConnectionsPerFile() int { return l.n }

type plainSource struct{}

func (plainSource) Size() int64 { return 1 }
func (plainSource) ReadAt(context.Context, int64, int) ([]byte, error) {
	return []byte{0}, nil
}

func TestAStorageWithoutALimitIsReadAndServedAsBefore(t *testing.T) {
	src := plainSource{}
	if got := gatedSource(struct{}{}, "/a.mkv", src); got != any(src) {
		t.Errorf("a storage with no limit got a wrapped source: %T", got)
	}
	if got := gatedSource(limitedStorage{0}, "/a.mkv", src); got != any(src) {
		t.Errorf("a limit of 0 is no limit, but the source was wrapped: %T", got)
	}
	if end := holdPlayback(struct{}{}, "/a.mkv"); end != nil {
		t.Error("a storage with no limit registered a playback")
	}
	if stream.DownloadGate.Playing("/a.mkv") {
		t.Error("the gate saw a playback of a file on a storage with no limit")
	}
}

func TestAStorageWithALimitGatesBackgroundReadsAndRegistersPlaybacks(t *testing.T) {
	st := limitedStorage{1}
	got, ok := gatedSource(st, "/g/a.mkv", plainSource{}).(*stream.GatedSource)
	if !ok || got.Key != "/g/a.mkv" || got.Gate != stream.DownloadGate {
		t.Fatalf("gatedSource: %#v", got)
	}
	end := holdPlayback(st, "/g/a.mkv")
	if end == nil || !stream.DownloadGate.Playing("/g/a.mkv") {
		t.Fatal("a playback on a limited storage is not registered")
	}
	end()
}
