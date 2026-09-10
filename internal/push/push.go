package push

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"chinamobile-monitor/internal/store"
)

var client = &http.Client{Timeout: 15 * time.Second}

// Bark 推送（iOS Bark App）
func bark(key, title, body string) error {
	if key == "" {
		return fmt.Errorf("未配置 Bark Key")
	}
	url := "https://api.day.app/" + key
	if strings.HasPrefix(key, "http://") || strings.HasPrefix(key, "https://") {
		url = key // 自建 Bark 服务器
	}
	payload := map[string]string{"title": title, "body": body}
	b, _ := json.Marshal(payload)
	resp, err := client.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Bark 返回状态码 %d", resp.StatusCode)
	}
	return nil
}

// PushPlus 推送（微信公众号）
func pushplus(token, title, content string) error {
	if token == "" {
		return fmt.Errorf("未配置 PushPlus Token")
	}
	payload := map[string]interface{}{
		"token":    token,
		"title":    title,
		"content":  content,
		"template": "txt",
		"channel":  "wechat",
	}
	b, _ := json.Marshal(payload)
	resp, err := client.Post("https://www.pushplus.plus/send", "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if out.Code != 200 {
		return fmt.Errorf("PushPlus: %s", out.Msg)
	}
	return nil
}

// SendAll 向所有已启用的渠道推送，返回成功渠道数
func SendAll(cfg store.PushSettings, title, body string) int {
	n := 0
	if cfg.BarkEnabled && cfg.BarkKey != "" {
		if err := bark(cfg.BarkKey, title, body); err == nil {
			n++
		}
	}
	if cfg.PushPlusEnabled && cfg.PushPlusToken != "" {
		if err := pushplus(cfg.PushPlusToken, title, body); err == nil {
			n++
		}
	}
	return n
}

// HasChannel 是否至少启用了一个可用的推送渠道
func HasChannel(cfg store.PushSettings) bool {
	return (cfg.BarkEnabled && cfg.BarkKey != "") || (cfg.PushPlusEnabled && cfg.PushPlusToken != "")
}

// Send2FACode 通过指定渠道推送面板登录验证码（channel: "bark" | "pushplus"）
func Send2FACode(cfg store.PushSettings, channel, code string) error {
	title := "话费监控验证码 " + code
	body := fmt.Sprintf("话费监控面板提醒你：\n正在执行登录操作\n验证码为：%s\n验证码有效期 5 分钟，请尽快认证", code)
	var err error
	switch channel {
	case "bark":
		if !cfg.BarkEnabled || cfg.BarkKey == "" {
			return fmt.Errorf("Bark 渠道未启用或未配置 Key")
		}
		err = bark(cfg.BarkKey, title, body)
	case "pushplus":
		if !cfg.PushPlusEnabled || cfg.PushPlusToken == "" {
			return fmt.Errorf("PushPlus 渠道未启用或未配置 Token")
		}
		err = pushplus(cfg.PushPlusToken, title, body)
	default:
		return fmt.Errorf("未知的推送渠道")
	}
	return err
}

// SendTest 测试推送
func SendTest(cfg store.PushSettings) error {
	title := "【话费监控】"
	body := "如果你收到这条消息，说明推送配置成功。"
	if SendAll(cfg, title, body) == 0 {
		return fmt.Errorf("推送失败，请检查配置")
	}
	return nil
}
