// Package netx 网络工具：Docker 容器（飞牛OS 等）可能 IPv6 不通但 DNS 返回了 AAAA 记录，
// Go 默认会优先尝试 IPv6 导致 TLS 握手超时。本包提供强制 IPv4 的拨号函数。
package netx

import (
	"context"
	"fmt"
	"net"
	"time"
)

// IPv4DialContext 强制 IPv4 拨号（解析主机名后只连接 A 记录地址）。
// 可直接赋给 http.Transport.DialContext。
func IPv4DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	// host 本身是 IP 时直接拨号
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return nil, fmt.Errorf("skip ipv6 address %s", host)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", net.JoinHostPort(host, port))
	}
	// 解析主机名，只取 IPv4 地址
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	d.Timeout = 10 * time.Second
	var lastErr error
	for _, ip := range ips {
		if ip.IP.To4() == nil {
			continue
		}
		conn, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no ipv4 address resolved for %s", host)
}
