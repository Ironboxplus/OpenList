package net

import (
	"crypto/tls"
	stdnet "net"
	"net/http"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

func NewOSSClient(endpoint, accessKeyID, accessKeySecret string, options ...oss.ClientOption) (*oss.Client, error) {
	clientOptions := []oss.ClientOption{oss.HTTPClient(NewOSSUploadHttpClient())}
	clientOptions = append(clientOptions, options...)
	return oss.New(endpoint, accessKeyID, accessKeySecret, clientOptions...)
}

func NewOSSUploadHttpClient() *http.Client {
	transport := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: conf.Conf.TlsInsecureSkipVerify},
		DialContext: (&stdnet.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 5 * time.Minute,
	}

	SetProxyIfConfigured(transport)

	return &http.Client{
		Timeout:   time.Hour * 48,
		Transport: transport,
	}
}
