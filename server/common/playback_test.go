package common

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/stream"
)

type limitedDriver struct{ n int }

func (l limitedDriver) MaxDownloadConnectionsPerFile() int { return l.n }

func TestHoldPlaybackRegistersOnlyForAStorageWithALimit(t *testing.T) {
	if end := HoldPlayback(struct{}{}, "/hold/none.mkv"); end != nil {
		t.Error("a storage with no limit registered a playback")
	}
	if end := HoldPlayback(limitedDriver{0}, "/hold/zero.mkv"); end != nil {
		t.Error("a limit of 0 is no limit, but a playback was registered")
	}
	if stream.DownloadGate.Playing("/hold/none.mkv") || stream.DownloadGate.Playing("/hold/zero.mkv") {
		t.Error("the gate saw a playback of a file on a storage with no limit")
	}

	end := HoldPlayback(limitedDriver{2}, "/hold/two.mkv")
	if end == nil || !stream.DownloadGate.Playing("/hold/two.mkv") {
		t.Fatal("a playback on a limited storage is not registered")
	}
	end()
	end() // ending twice is harmless
}
