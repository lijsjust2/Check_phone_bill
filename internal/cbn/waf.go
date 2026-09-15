package cbn

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// 阿里云 WAF acw_sc__v2 JS 挑战求解：挑战页内嵌混淆脚本执行后
// 通过 document.cookie 写入 acw_sc__v2=<值>。goja 真实执行该脚本取值。

var (
	reScript   = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)
	reTextarea = regexp.MustCompile(`(?s)<textarea[^>]*id="renderData"[^>]*>(.*?)</textarea>`)
)

// IsWAFChallenge 判断响应是否为 WAF 挑战页
func IsWAFChallenge(body string) bool {
	return strings.Contains(body, "acw_sc__v2") && strings.Contains(body, "renderData")
}

// SolveWAFChallenge 在挑战页 HTML 中执行挑战脚本，返回 acw_sc__v2 cookie 值
func SolveWAFChallenge(html, pageURL, ua string) (string, error) {
	ta := ""
	if m := reTextarea.FindStringSubmatch(html); m != nil {
		ta = htmlUnescapeJS(m[1])
	}
	if ta == "" {
		return "", fmt.Errorf("挑战页缺少 renderData")
	}

	vm := goja.New()
	doc := vm.NewObject()
	_ = doc.Set("cookie", "")
	elem := vm.NewObject()
	_ = elem.Set("innerHTML", ta)
	_ = doc.Set("getElementById", func(id string) *goja.Object { return elem })
	loc := vm.NewObject()
	_ = loc.Set("href", pageURL)
	_ = loc.Set("reload", func() {})
	_ = doc.Set("location", loc)
	_ = doc.Set("referrer", "")
	_ = vm.Set("document", doc)
	_ = vm.Set("location", loc)
	_ = vm.Set("window", vm.GlobalObject())
	nav := vm.NewObject()
	_ = nav.Set("userAgent", ua)
	_ = vm.Set("navigator", nav)
	// 挑战脚本用 setTimeout 触发 reload/写 cookie，立即同步执行
	_ = vm.Set("setTimeout", func(args ...goja.Value) {
		if len(args) > 0 {
			if f, ok := goja.AssertFunction(args[0]); ok {
				_, _ = f(goja.Undefined())
			}
		}
	})
	_ = vm.Set("setInterval", func() {})
	_ = vm.Set("atob", func(s string) string {
		s = strings.TrimSpace(s)
		if d, err := base64.StdEncoding.DecodeString(s); err == nil {
			return string(d)
		}
		if d, err := base64.RawStdEncoding.DecodeString(s); err == nil {
			return string(d)
		}
		return ""
	})
	_ = vm.Set("btoa", func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) })
	// goja 未实现 legacy Date.toGMTString/toUTCString（挑战脚本 setCookie 用到）
	if dv, ok := vm.Get("Date").(*goja.Object); ok {
		if pv, ok := dv.Get("prototype").(*goja.Object); ok {
			gmt := func(call goja.FunctionCall) goja.Value {
				ms := call.This.ToNumber().ToInteger()
				return vm.ToValue(time.UnixMilli(ms).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
			}
			_ = pv.Set("toGMTString", gmt)
			_ = pv.Set("toUTCString", gmt)
		}
	}

	for _, m := range reScript.FindAllStringSubmatch(html, -1) {
		code := strings.TrimSpace(m[1])
		if code == "" {
			continue
		}
		if _, err := vm.RunString(code); err != nil {
			return "", fmt.Errorf("挑战脚本执行失败: %w", err)
		}
		if v := cookieValueJS(doc.Get("cookie").String()); v != "" {
			return v, nil
		}
	}
	if v := cookieValueJS(doc.Get("cookie").String()); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("挑战脚本执行后未生成 acw_sc__v2")
}

func cookieValueJS(s string) string {
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "acw_sc__v2=") {
			return strings.TrimPrefix(part, "acw_sc__v2=")
		}
	}
	return ""
}

func htmlUnescapeJS(s string) string {
	r := strings.NewReplacer("&quot;", `"`, "&#39;", "'", "&amp;", "&", "&lt;", "<", "&gt;", ">")
	return r.Replace(s)
}
