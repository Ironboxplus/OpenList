package _115_share

import (
	"encoding/json"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
)

func TestDownloadConnectionsPerFileIsASettingThatDefaultsToTwo(t *testing.T) {
	// a storage saved before the setting existed
	if n := driver.DownloadConnectionsPerFile(&Pan115Share{}); n != 2 {
		t.Fatalf("a storage with no setting allows %d connections per file, want 2", n)
	}
	var saved Pan115Share
	if err := json.Unmarshal([]byte(`{"cookie":"c"}`), &saved.Addition); err != nil {
		t.Fatal(err)
	}
	if n := driver.DownloadConnectionsPerFile(&saved); n != 2 {
		t.Fatalf("addition without the field: %d, want 2", n)
	}
	for _, c := range []struct {
		json string
		want int
	}{{`{"max_conns_per_file":1}`, 1}, {`{"max_conns_per_file":3}`, 3}, {`{"max_conns_per_file":0}`, 0}} {
		var o Pan115Share
		if err := json.Unmarshal([]byte(c.json), &o.Addition); err != nil {
			t.Fatal(err)
		}
		if n := driver.DownloadConnectionsPerFile(&o); n != c.want {
			t.Errorf("%s: %d, want %d", c.json, n, c.want)
		}
	}
}
