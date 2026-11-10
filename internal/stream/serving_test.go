package stream

import (
	"testing"
	"time"
)

func TestServedWithinSeesOnlyWhatWasMarkedRecently(t *testing.T) {
	if ServedWithin("/never", time.Hour) {
		t.Fatal("a path never marked is served")
	}
	MarkServing("/a/b.mkv")
	if !ServedWithin("/a/b.mkv", time.Minute) {
		t.Error("a path just marked is not served")
	}
	time.Sleep(20 * time.Millisecond)
	if ServedWithin("/a/b.mkv", 10*time.Millisecond) {
		t.Error("the grace is over and the path is still served")
	}
	if ServedWithin("/a/c.mkv", time.Minute) {
		t.Error("another path is served")
	}
}
