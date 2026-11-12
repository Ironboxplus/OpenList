package net

import (
	"context"
	"crypto/tls"
	stdnet "net"
	"net/http"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// The OSS SDK's own transport gives every read and write on its connections a
// deadline (aliyun-oss-go-sdk oss/conn.go timeoutConn, transport_1_7.go;
// ReadWriteTimeout 60 s, LongTimeout 300 s). The client below replaces that
// transport, so it does the same: without it a part whose connection stops
// taking bytes waits forever (ResponseHeaderTimeout only starts once the body
// is written), and the upload's retry never comes.
var (
	ossIOTimeout   = 60 * time.Second  // one read or write making no progress
	ossLongTimeout = 300 * time.Second // the connection idle between them
)

// ossTimeoutConn is the SDK's timeoutConn.
type ossTimeoutConn struct {
	stdnet.Conn
	timeout, longTimeout time.Duration
}

func newOSSTimeoutConn(c stdnet.Conn, timeout, longTimeout time.Duration) *ossTimeoutConn {
	c.SetReadDeadline(time.Now().Add(longTimeout))
	return &ossTimeoutConn{Conn: c, timeout: timeout, longTimeout: longTimeout}
}

func (c *ossTimeoutConn) Read(b []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	n, err := c.Conn.Read(b)
	c.Conn.SetReadDeadline(time.Now().Add(c.longTimeout))
	return n, err
}

func (c *ossTimeoutConn) Write(b []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	n, err := c.Conn.Write(b)
	c.Conn.SetReadDeadline(time.Now().Add(c.longTimeout))
	return n, err
}

func NewOSSClient(endpoint, accessKeyID, accessKeySecret string, options ...oss.ClientOption) (*oss.Client, error) {
	// NewOSSUploadHttpClient has the longer upload timeouts; the guard against
	// cloud metadata addresses (safeTransport) is the same as NewHttpClient's.
	httpClient := NewOSSUploadHttpClient()
	httpClient.Transport = &safeTransport{base: httpClient.Transport}
	clientOptions := []oss.ClientOption{oss.HTTPClient(httpClient)}
	clientOptions = append(clientOptions, options...)
	return oss.New(endpoint, accessKeyID, accessKeySecret, clientOptions...)
}

func NewOSSUploadHttpClient() *http.Client {
	dialer := &stdnet.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	timeout, longTimeout := ossIOTimeout, ossLongTimeout
	transport := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: conf.Conf.TlsInsecureSkipVerify},
		DialContext: func(ctx context.Context, network, addr string) (stdnet.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return newOSSTimeoutConn(c, timeout, longTimeout), nil
		},
		ResponseHeaderTimeout: 5 * time.Minute,
	}

	SetProxyIfConfigured(transport)

	return &http.Client{
		Timeout:   time.Hour * 48,
		Transport: transport,
	}
}
