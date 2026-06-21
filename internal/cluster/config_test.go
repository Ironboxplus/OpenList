package cluster

import "testing"

func TestConfigActive(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"disabled", Config{Enabled: false, Key: "k", Peers: []string{"http://p"}}, false},
		{"no key", Config{Enabled: true, Peers: []string{"http://p"}}, false},
		{"no peers", Config{Enabled: true, Key: "k"}, false},
		{"blank peers only", Config{Enabled: true, Key: "k", Peers: []string{"  ", "/"}}, false},
		{"active", Config{Enabled: true, Key: "k", Peers: []string{"http://p"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.active(); got != c.want {
				t.Fatalf("active() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestConfigShouldShare(t *testing.T) {
	cfg := Config{ShareDrivers: []string{"115 Open"}, ShareMounts: []string{"/115"}}
	if !cfg.shouldShare("115 Open", "/115") {
		t.Fatal("in-scope storage should be shared")
	}
	if cfg.shouldShare("Local", "/115") {
		t.Fatal("driver out of filter must not be shared")
	}
	if cfg.shouldShare("115 Open", "/other") {
		t.Fatal("mount out of filter must not be shared")
	}

	// Empty filters => share everything.
	open := Config{}
	if !open.shouldShare("AnyDriver", "/anywhere") {
		t.Fatal("empty filters should share everything")
	}
}

func TestConfigPeerListNormalizes(t *testing.T) {
	cfg := Config{Peers: []string{" http://a/ ", "http://b", "", "  "}}
	got := cfg.peerList()
	if len(got) != 2 || got[0] != "http://a" || got[1] != "http://b" {
		t.Fatalf("peerList normalize failed: %#v", got)
	}
}
