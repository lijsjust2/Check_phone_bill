// Package netx 网络工具：Docker 容器（飞牛OS 等）常见两类出口问题——
//  1. IPv6 不通但 DNS 返回 AAAA 记录，Go 默认优先尝试 IPv6 导致超时；
//  2. 容器网桥 MTU 1500 大于实际路径 MTU（PPPoE 1492 / VPN 隧道更小），
//     大包被静默丢弃：小包（TCP 握手/DNS）正常，TLS 证书链与登录报文黑洞，
//     表现为「TLS handshake timeout」或「awaiting headers 超时」。
//
// IPv4DialContext 同时解决两者：强制 IPv4 + 钳制 TCP MSS（仅 Linux）。
package netx

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

// socketControl 平台相关 socket 选项钩子（Linux 下钳制 TCP MSS，见 mss_linux.go）；
// 其他平台为 nil，net.Dialer 允许 Control 为 nil。
var socketControl func(network, address string, c syscall.RawConn) error

// IPv4DialContext 强制 IPv4 拨号（解析主机名后只连接 A 记录地址）。
// 可直接赋给 http.Transport.DialContext。
func IPv4DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 10 * time.Second, Control: socketControl}
	// host 本身是 IP 时直接拨号
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return nil, fmt.Errorf("skip ipv6 address %s", host)
		}
		return d.DialContext(ctx, "tcp4", net.JoinHostPort(host, port))
	}
	// 解析主机名，只取 IPv4 地址
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
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
