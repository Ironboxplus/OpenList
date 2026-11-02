package rpc

import (
	"net"
	"testing"
	"time"
)

// requireAria2 skips the test unless an aria2 daemon is listening on its
// default RPC port; these tests talk to a real daemon (localhost:6800) and
// download from the internet, so they only run where one is provided.
func requireAria2(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "localhost:6800", time.Second)
	if err != nil {
		t.Skipf("needs an aria2 daemon on localhost:6800: %v", err)
	}
	_ = conn.Close()
}
