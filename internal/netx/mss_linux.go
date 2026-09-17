//go:build linux

package netx

import "syscall"

// 钳制 TCP MSS：Docker 网桥默认 MTU 1500，宿主机走 PPPoE（路径 MTU 1492）或
// VPN 隧道时，接近 1500 的包被静默丢弃——TCP 握手/DNS 等小包正常，
// TLS 证书链、登录报文等大包黑洞，表现为「TLS handshake timeout」或
// 「awaiting headers 超时」。连接建立前把 MSS 钳到 1300，
// 所有报文均低于常见路径 MTU（PPPoE 1492 / WireGuard 1420 等）。
func init() {
	socketControl = func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, 1300)
		})
		if err != nil {
			return err
		}
		return serr
	}
}
