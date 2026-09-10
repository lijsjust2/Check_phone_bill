// Package cbn 中国广电（中国广播电视网络集团，10099）接入实现。
// 协议逆向自网上营业厅 www.10099.com.cn 前端（common-f0160.js）：
//   - 请求体 RSA 加密（公钥 encryptLong 分块）、Header Access=MD5(参数排序拼接)
//   - 响应密文用前端泄漏的私钥 decryptLong 解密
//   - 阿里云 WAF acw_sc__v2 JS 挑战（waf.go 用 goja 求解）
//   - 登录：图片验证码 → 短信验证码（loginType "5"）→ gwLogin → 服务端 sessionId
//   - 查询：Cookie 会话 + sessionId（qryBalanceFee / qryPhonePackInfo 等）
package cbn

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// 前端泄漏的 RSA 密钥（提取自 www.10099.com.cn/js/common-f0160.js，JSEncrypt 3.2.1）
const (
	pubKeyB64 = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA4QJmWsYFtBbd/yoySbBAy7pBbfrgHr2SGSOGoBukd65IUqQ9uWhvzCN9r0nWESIXWW6FR2A5adHUFFeETucXGjU61BZxbLLf0ARL4F1NkkFMUF/D3GMY401+6TK8jVMxy4vLb6EKzlyZaI8jKBJ9jh0HMli6U7JHydsXstGRUnyx8Mu11bJ3+ZMUcKe51Zi+85Ez756EdZhGTXSY7pUAvh8/0Fea6mtsOs9OLMHbGMKAOE0alN1QdfqE3QMKgB58MVwlPY7uljelSocjNxCIS58CLvu1iWFrLvsCp8t3DavyA1OD/PPcXRrNLYZgzG5304/LAqfurOpU35AaB5tOnwIDAQAB"

	privKeyB64 = "MIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQDhAmZaxgW0Ft3/KjJJsEDLukFt+uAevZIZI4agG6R3rkhSpD25aG/MI32vSdYRIhdZboVHYDlp0dQUV4RO5xcaNTrUFnFsst/QBEvgXU2SQUxQX8PcYxjjTX7pMryNUzHLi8tvoQrOXJlojyMoEn2OHQcyWLpTskfJ2xey0ZFSfLHwy7XVsnf5kxRwp7nVmL7zkTPvnoR1mEZNdJjulQC+Hz/QV5rqa2w6z04swdsYwoA4TRqU3VB1+oTdAwqAHnwxXCU9ju6WN6VKhyM3EIhLnwIu+7WJYWsu+wKny3cNq/IDU4P889xdGs0thmDMbnfTj8sCp+6s6lTfkBoHm06fAgMBAAECggEAdwFz7TKqtZMamuhQbJTh0F6UWHzFqLyO1ujpPSkhlYMCEWN4meVYq9lhkiI1LB6hxtUjfJqyAvvNdWzMN4cVuvDISoAMQXdh1H1RPDtc2avblu7vglKPSTkllGUXQI/t2D/5uvKr6nUjVh/OclVFPrKvqbsv4TB7s5FDOXqJp9v5jhJOs8fmS1eA5wL4SuvwfXqxVU+e7DCL21hWICLsFLpysvsmfcToPMZ9og7KZG4nHkEBMTZ6HVBS3RnayFcF6Q7kIqkKrIeIFb88R81PnH1xXhBr9k7Mgm6l+wxtWRhhdQsjDY542c1Buh7eBcE7rdClUPucyyv3w9vKgDngIQKBgQD6YcWcguASxL2IYSIdZNCqEey0I8MdPsJ5OfCrkrOQnEUCQCLEyZUcfGxr1+VsGvIfNSLVLiZJVEvB7tsUb+OLmlElDycMfKzvAPEsbh/UKryeAZF4VJeNOeYuT10z2OMxsxh1tmkOdobaK4eIUxr7CpUNa2v2DDYithB0GshzyQKBgQDmDuJve7+bFSbB6aZCuJFnIcF1pgGR/jiTbMqJmvm8LVNpu8SQdd1fQX1/szOjtzfiXGWrlkBNQIlYOi2iWnyRBQYHXQeji/zcCIJCTXDsEYtFj3A4ovJZUUZW7N0aUq7h+zQCWZ2H/Iq8SPeA1OnGJV78CAzLJ/kcjZd4sgvTJwKBgGIBpW1nGTifhCT/CHCDBt6bV5EHspce+tai5F70dI81bBm+ax2mXlShK3tnLemL/pxSm0jg4KGxeln2GhE83s/FXt/nt3w+zR5cuwqOLK1K8TvUF1IHoq7oK/6SmEP0MLJCjV9+QE8l/BEoGsw044nCkaeIFeFg1EvwAi7AURhpAoGASgJBz/F0a1R7mmgq503u4MmYLdvQp4Gr+6lE4s2rR2Ehc2NHUd3I8GrmD527oBBB9x0YTAHS/8ciJ/LXWWJYrmJ6VQYVfgR7vOEz3laBXEAsmJ0TUfUBl8Awq6gZXO16exJP4e2oYuXYT8f9b0GPTwIYs2V3kCd02T2nm9lTOoMCgYB9sbBXCnnuht0TCA88cZtSKDRa1YtgdYCjSq6xClvhnKa1bh+Ldfp5GVF4CLsEvgHgQS5QWh6pWO6wH0GOrr13SdgT50AwInmpdvqnD37kP1goyMtITiKxI6gNwbSsDMaGpJNSGzHFQR1Wy3iQ+r6XI/pSFZDhK6PFjUVVwbRFQw=="
)

var (
	rsaPub  *rsa.PublicKey
	rsaPriv *rsa.PrivateKey
	keyOnce sync.Once
	keyErr  error
)

func loadKeys() error {
	keyOnce.Do(func() {
		pb, err := base64.StdEncoding.DecodeString(pubKeyB64)
		if err != nil {
			keyErr = fmt.Errorf("公钥解码失败: %w", err)
			return
		}
		k, err := x509.ParsePKIXPublicKey(pb)
		if err != nil {
			keyErr = fmt.Errorf("公钥解析失败: %w", err)
			return
		}
		rsaPub = k.(*rsa.PublicKey)
		vb, err := base64.StdEncoding.DecodeString(privKeyB64)
		if err != nil {
			keyErr = fmt.Errorf("私钥解码失败: %w", err)
			return
		}
		k2, err := x509.ParsePKCS8PrivateKey(vb)
		if err != nil {
			keyErr = fmt.Errorf("私钥解析失败: %w", err)
			return
		}
		rsaPriv = k2.(*rsa.PrivateKey)
	})
	return keyErr
}

// SignAccess 复刻前端 u()+h()：参数键排序拼 k=v&...（对象降一层展开，
// 空串/null/数组丢弃）后取 MD5 hex 作为 Access 头。
func SignAccess(params map[string]interface{}) (string, error) {
	if err := loadKeys(); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch v := params[k].(type) {
		case string:
			if v != "" {
				parts = append(parts, k+"="+v)
			}
		case int64:
			parts = append(parts, k+"="+strconv.FormatInt(v, 10))
		case float64:
			parts = append(parts, k+"="+strconv.FormatFloat(v, 'f', -1, 64))
		case bool:
			parts = append(parts, k+"="+strconv.FormatBool(v))
		}
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&")))
	return hex.EncodeToString(sum[:]), nil
}

// EncryptLong 复刻 JSEncrypt 3.2.1 encryptLong：明文按 RSA 块长-11 分块
// PKCS1v15 加密，密文拼接后整体 base64。
func EncryptLong(s string) (string, error) {
	if err := loadKeys(); err != nil {
		return "", err
	}
	chunk := (rsaPub.N.BitLen()+7)/8 - 11
	data := []byte(s)
	var out []byte
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		ct, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, data[i:end])
		if err != nil {
			return "", fmt.Errorf("RSA 加密失败: %w", err)
		}
		out = append(out, ct...)
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// DecryptLong 复刻 decryptLong：base64 → 按 RSA 块长分块解密拼接。
func DecryptLong(b64 string) (string, error) {
	if err := loadKeys(); err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("密文 base64 解码失败: %w", err)
	}
	size := rsaPriv.Size()
	var out []byte
	for i := 0; i < len(raw); i += size {
		end := i + size
		if end > len(raw) {
			end = len(raw)
		}
		pt, err := rsa.DecryptPKCS1v15(rand.Reader, rsaPriv, raw[i:end])
		if err != nil {
			return "", fmt.Errorf("RSA 解密失败: %w", err)
		}
		out = append(out, pt...)
	}
	return string(out), nil
}

// EncodePayload 请求载荷加密链：encryptLong(encodeURIComponent(JSON(params)))
func EncodePayload(params map[string]interface{}) (string, error) {
	jb, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	return EncryptLong(encodeURIComponent(string(jb)))
}

// DecodePayload 响应明文链：decodeURIComponent(解密文本)
func DecodePayload(pt string) string {
	return decodeURIComponent(pt)
}

// encodeURIComponent JS encodeURIComponent 语义（多字节字符逐字节 %XX）
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			strings.IndexByte("-_.!*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// decodeURIComponent JS decodeURIComponent 语义（%XX 解码，容错保留原样）
func decodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// GetUUID 复刻前端 getUUID：标准 UUID v4 格式（36 位含连字符）
func GetUUID() string {
	const hexd = "0123456789abcdef"
	b := make([]byte, 36)
	for i := range b {
		b[i] = hexd[randByte()%16]
	}
	b[14] = '4'
	b[19] = hexd[3&randByte()|8]
	b[8], b[13], b[18], b[23] = '-', '-', '-', '-'
	return string(b)
}

func randByte() byte {
	var b [1]byte
	_, _ = rand.Read(b[:])
	return b[0]
}