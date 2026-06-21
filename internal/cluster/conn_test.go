package cluster

import "testing"

func TestWSURL(t *testing.T) {
	cases := map[string]string{
		"https://node.example.com":  "wss://node.example.com" + wsPath,
		"http://10.0.0.2:5244":      "ws://10.0.0.2:5244" + wsPath,
		"https://node.example.com/": "wss://node.example.com" + wsPath,
		"  http://h:1/  ":           "ws://h:1" + wsPath,
		"node.example.com":          "ws://node.example.com" + wsPath,
		"wss://already.example.com": "wss://already.example.com" + wsPath,
	}
	for in, want := range cases {
		if got := wsURL(in); got != want {
			t.Errorf("wsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConnRegistry(t *testing.T) {
	r := newConnRegistry()
	c1 := &peerConn{closed: make(chan struct{})}
	c2 := &peerConn{closed: make(chan struct{})}
	r.add(c1)
	r.add(c2)
	if len(r.all()) != 2 {
		t.Fatalf("expected 2 conns, got %d", len(r.all()))
	}
	r.bind(c1, "NODE1")
	if !r.hasNode("NODE1") {
		t.Fatal("hasNode should find a bound node")
	}
	r.remove(c1)
	if r.hasNode("NODE1") {
		t.Fatal("removed conn's node id should be gone")
	}
	if len(r.all()) != 1 {
		t.Fatalf("expected 1 conn after remove, got %d", len(r.all()))
	}
}
