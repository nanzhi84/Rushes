package providers

import (
	"context"
	"net"
	"net/http"
	"time"
)

// NewIPv4Client 为国内模型端点强制 IPv4、禁用系统代理，并把超时统一放在 client 上。
func NewIPv4Client(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp4", address)
			},
			ForceAttemptHTTP2: true,
		},
	}
}
