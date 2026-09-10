package telecom

import (
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// 电信网关（appgologin.189.cn / appfuwu.189.cn）TLS 配置老旧：
// 放开 TLS 1.0 与 RSA 密钥交换（Go 1.22+ 默认禁用），全部隔离在本文件。
func init() {
	enableGodebug("tlsrsakex=1") // 允许 RSA 密钥交换套件（运行时生效，仅影响本进程）
}

func enableGodebug(setting string) {
	v := os.Getenv("GODEBUG")
	if v == "" {
		_ = os.Setenv("GODEBUG", setting)
	} else if !strings.Contains(v, strings.SplitN(setting, "=", 2)[0]) {
		_ = os.Setenv("GODEBUG", v+","+setting)
	}
}

// newClient 电信接口专用 HTTP 客户端（旧 TLS 兼容）
func newClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS10,
				MaxVersion:         tls.VersionTLS13,
				InsecureSkipVerify: false,
				CipherSuites: []uint16{
					// TLS 1.0/1.1（旧网关可能仍依赖）
					tls.TLS_RSA_WITH_AES_128_CBC_SHA,
					tls.TLS_RSA_WITH_AES_256_CBC_SHA,
					tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
					tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
					// TLS 1.2+
					tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
					tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
				},
			},
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			ResponseHeaderTimeout: 20 * time.Second,
		},
	}
}

var httpClient = newClient()
