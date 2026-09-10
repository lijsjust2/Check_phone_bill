package unicom

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveWebSendMsgConnectivity 官网 SendMSG 接口连通性验证（默认跳过）。
// 设置 UNICOM_LIVE=1 启用：不带滑块票据请求接口，返回 resultCode=7001（滑块校验失败）
// 即证明接口连通且服务端强制校验腾讯滑块（这是采用网关登录方案的原因）。
func TestLiveWebSendMsgConnectivity(t *testing.T) {
	if os.Getenv("UNICOM_LIVE") == "" {
		t.Skip("设置 UNICOM_LIVE=1 启用联通官网接口连通性测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, err := probeWebSendMsg(ctx, "12345")
	if err != nil {
		t.Fatalf("请求失败（协议不通）: %v", err)
	}
	code := ""
	if i := strings.Index(body, "resultCode\":\""); i >= 0 {
		rest := body[i+len("resultCode\":\""):]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			code = rest[:j]
		}
	}
	t.Logf("响应: %s（resultCode=%s，7001=需滑块票据，协议连通正常）", body, code)
}
