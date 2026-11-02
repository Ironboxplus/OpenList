package db

import (
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestTouchEmbyDeviceWritesAtMostOncePerInterval(t *testing.T) {
	testDB(t)
	clock := time.Unix(1_800_000_000, 0)
	oldNow := embyNow
	embyNow = func() time.Time { return clock }
	t.Cleanup(func() {
		embyNow = oldNow
		embyTouched.Range(func(k, _ any) bool { embyTouched.Delete(k); return true })
	})

	d := &model.EmbyDevice{UserID: 1, DeviceID: "dev", TokenHash: "h1"}
	if err := CreateEmbyDevice(d); err != nil {
		t.Fatal(err)
	}
	seen := func() time.Time {
		var got model.EmbyDevice
		if err := db.First(&got, d.ID).Error; err != nil {
			t.Fatal(err)
		}
		return got.LastSeenAt
	}

	TouchEmbyDevice(d.ID)
	first := seen()
	if !first.Equal(clock) {
		t.Fatalf("first touch wrote %v, want %v", first, clock)
	}
	for i := 0; i < 100; i++ {
		clock = clock.Add(200 * time.Millisecond)
		TouchEmbyDevice(d.ID)
	}
	if got := seen(); !got.Equal(first) {
		t.Fatalf("100 touches inside the interval wrote again: %v -> %v", first, got)
	}
	clock = clock.Add(embyTouchInterval)
	TouchEmbyDevice(d.ID)
	if got := seen(); !got.Equal(clock) {
		t.Fatalf("touch after the interval did not write: %v, want %v", got, clock)
	}

	// another device has its own interval
	o := &model.EmbyDevice{UserID: 1, DeviceID: "other", TokenHash: "h2"}
	if err := CreateEmbyDevice(o); err != nil {
		t.Fatal(err)
	}
	TouchEmbyDevice(o.ID)
	var got model.EmbyDevice
	if err := db.First(&got, o.ID).Error; err != nil || !got.LastSeenAt.Equal(clock) {
		t.Fatalf("second device: %v %v", got.LastSeenAt, err)
	}
}
