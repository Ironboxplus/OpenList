package net

import (
	"bytes"
	stdnet "net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
)

func TestNewOSSUploadHttpClientHasLongerTimeout(t *testing.T) {
	oldConf := conf.Conf
	conf.Conf = conf.DefaultConfig("data")
	defer func() { conf.Conf = oldConf }()

	client := NewOSSUploadHttpClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.Transport)
	}
	if transport.ResponseHeaderTimeout < 120_000_000_000 { // 2 minutes minimum
		t.Fatalf("ResponseHeaderTimeout=%v, want >= 2m for upload", transport.ResponseHeaderTimeout)
	}
}

func TestNewOSSClientUsesEnvironmentHTTPSProxy(t *testing.T) {
	oldConf := conf.Conf
	conf.Conf = conf.DefaultConfig("data")
	defer func() {
		conf.Conf = oldConf
	}()

	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	t.Setenv("https_proxy", "http://127.0.0.1:7890")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	client, err := NewOSSClient("https://oss-cn-hangzhou.aliyuncs.com", "test-access-key", "test-access-secret")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if client.HTTPClient == nil {
		t.Fatal("expected OSS client to use a custom HTTP client")
	}

	safe, ok := client.HTTPClient.Transport.(*safeTransport)
	if !ok {
		t.Fatalf("expected *safeTransport, got %T", client.HTTPClient.Transport)
	}
	transport, ok := safe.base.(*http.Transport)
	if !ok {
		t.Fatalf("expected safe transport to wrap *http.Transport, got %T", safe.base)
	}

	if transport.Proxy == nil {
		t.Fatal("expected proxy function to be configured")
	}

	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "oss-cn-hangzhou.aliyuncs.com"}}
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("expected no proxy lookup error, got %v", err)
	}
	if proxyURL == nil {
		t.Fatal("expected HTTPS proxy to be used")
	}
	if got, want := proxyURL.String(), "http://127.0.0.1:7890"; got != want {
		t.Fatalf("expected proxy %q, got %q", want, got)
	}
}

// An upload whose connection stops taking bytes fails once a write has made no
// progress for ossIOTimeout, as with the SDK's own transport, instead of
// holding its caller forever (retrieval medical-to-115, 2026-11-12: four
// UploadPart requests stuck 8 to 10 minutes in persistConn.roundTrip held every
// Cfs transfer slot, and nothing more reached 115).
func TestOSSUploadClientGivesUpOnAStalledConnection(t *testing.T) {
	oldConf := conf.Conf
	conf.Conf = conf.DefaultConfig("data")
	defer func() { conf.Conf = oldConf }()
	oldTimeout := ossIOTimeout
	ossIOTimeout = 300 * time.Millisecond
	defer func() { ossIOTimeout = oldTimeout }()

	// A server that accepts the connection and never reads from it.
	ln, err := stdnet.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var held []stdnet.Conn
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()

	client := NewOSSUploadHttpClient()
	client.Transport.(*http.Transport).Proxy = nil
	req, err := http.NewRequest(http.MethodPut, "http://"+ln.Addr().String()+"/part", bytes.NewReader(make([]byte, 64<<20)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an upload to a connection that takes no bytes succeeded")
		}
		t.Logf("stalled upload: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the upload still waits 10s after its connection stopped taking bytes")
	}
}
