package cluster

import "testing"

func TestConfigActive(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"disabled", Config{Enabled: false, Key: "k"}, false},
		{"no key", Config{Enabled: true}, false},
		{"blank key", Config{Enabled: true, Key: "   "}, false},
		// A reachable node needs no peer/seed to be active — it can accept-only.
		{"active no seed", Config{Enabled: true, Key: "k"}, true},
		{"active with seed", Config{Enabled: true, Key: "k", Seeds: []string{"http://p"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.active(); got != c.want {
				t.Fatalf("active() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestConfigSeedListNormalizes(t *testing.T) {
	cfg := Config{Seeds: []string{" http://a/ ", "http://b", "", "  ", "http://a"}}
	got := cfg.seedList()
	if len(got) != 2 || got[0] != "http://a" || got[1] != "http://b" {
		t.Fatalf("seedList normalize/dedup failed: %#v", got)
	}
}

func TestConfigAnnounceIntervalDefault(t *testing.T) {
	if (Config{}).announceInterval() != defaultAnnounceIntervalSec {
		t.Fatal("zero interval should fall back to default")
	}
	if (Config{AnnounceIntervalSec: 10}).announceInterval() != 10 {
		t.Fatal("explicit interval should be honored")
	}
}
