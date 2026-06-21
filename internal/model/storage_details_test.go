package model

import (
	"encoding/json"
	"testing"
)

func TestStorageDetailsMarshalIncludesDriverName(t *testing.T) {
	d := StorageDetails{
		DiskUsage:  DiskUsage{TotalSpace: 100, UsedSpace: 40},
		DriverName: "BaiduNetdisk",
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["driver_name"] != "BaiduNetdisk" {
		t.Fatalf("expected driver_name in JSON, got %v", m["driver_name"])
	}
	if m["total_space"].(float64) != 100 || m["free_space"].(float64) != 60 {
		t.Fatalf("disk fields wrong: %v", m)
	}
}
