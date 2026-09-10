package telecom

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
)

// TransNumber 凯撒移位编码（encode +2 / decode -2）。
// 电信登录接口用它混淆手机号/密码（Python 版 trans_number 等价实现）。
func TransNumber(s string, encode bool) string {
	shift := 2
	if !encode {
		shift = -2
	}
	b := []rune(s)
	for i, c := range b {
		b[i] = rune((int(c) + shift) & 0xFFFF)
	}
	return string(b)
}

// 电信登录接口 RSA 公钥（Python 版硬编码公钥等价）
const loginPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDBkLT15ThVgz6/NOl6s8GNPofd
WzWbCkWnkaAm7O2LjkM1H7dMvzkiqdxU02jamGRHLX/ZNMCXHnPcW/sDhiFCBN18
qFvy8g6VYb9QtroI09e176s+ZCtiv7hbin2cCTj99iUpnEloZm19lwHyo69u5UMi
PMpq0/XKBO8lYhN/gwIDAQAB
-----END PUBLIC KEY-----`

var loginPubKey *rsa.PublicKey

func init() {
	block, _ := pem.Decode([]byte(loginPublicKeyPEM))
	if block == nil {
		panic("telecom: 登录公钥解析失败")
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rk, ok := key.(*rsa.PublicKey); ok {
			loginPubKey = rk
		}
	} else if rk, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		loginPubKey = rk
	}
	if loginPubKey == nil {
		panic("telecom: 登录公钥格式不支持")
	}
}

// EncryptRSA RSA PKCS1v15 加密后 base64（Python 版 PKCS1_v1_5 等价）
func EncryptRSA(plain string) (string, error) {
	out, err := rsa.EncryptPKCS1v15(rand.Reader, loginPubKey, []byte(plain))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out), nil
}
