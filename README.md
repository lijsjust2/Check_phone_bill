# 话费监控

四网（移动 / 联通 / 电信 / 广电）话费与套餐用量监控面板（Go 版）。单二进制 + Web 面板，支持多账号管理、定时查询、Bark/PushPlus 推送、2FA 登录保护。

## 功能

### 四网账号管理

- 中国移动：短信验证码登录（内置 Chromium 无头浏览器，网页端完成，无需命令行）
- 中国联通：网页浏览器登录（面板弹出真实浏览器窗口，滑块与短信验证码在窗口内完成；
  登录成功后保存 `JUT` Cookie，之后查询纯 HTTP 直连，不再开浏览器）
- 中国电信：服务密码登录（纯 HTTP 接口，token 长期有效，失效自动重登）
- 中国广电：短信验证码 + 图片验证码登录（浏览器网页端）
- 添加账号时手动选择运营商（携号转网以用户选择为准）
- 账号列表：备注、手机号（脱敏显示，点击查看完整号码）、运营商、状态、余额、通用流量、语音、查询时间
- 单号查询 / 一键查询全部，登录态持久化，一次登录长期使用（联通 JUT 失效会置为未登录并提示重登，电信 token 失效自动密码重登）

### 费用明细

- 三级下钻：每日汇总 → 当日各号码 → 单号码逐日明细
- 已用话费 = 相邻两次查询余额差值，已用流量同理，自动计算
- 每日快照保留 400 天

### 定时与推送

- 每日定点自动查询（时间可配置）
- Bark / PushPlus 推送查询结果
- 推送字段可勾选（全局默认 + 每账号独立覆盖）
- 仅告警时推送：余额低于阈值 / 流量用量超百分比

### 安全

- 面板登录 2FA：验证码通过 Bark 或 PushPlus 推送（二选一），5 分钟有效
- 登录失败次数过多自动封禁 IP
- scrypt 密码哈希 + CSRF 防护

### 其他

- Web 端服务端日志查看
- 备份导出 / 导入（zip，含账号、设置、登录态，导入后无需重新登录）
- 数据目录纯文件存储（store.json），易于迁移

## 部署

### Docker

```bash
docker run -d \
  --name chinamobile-monitor \
  -p 10086:10086 \
  -v ./data:/app/data \
  --shm-size 512m \
  -e TZ=Asia/Shanghai \
  --restart unless-stopped \
  chinamobile-monitor
```

或使用 `docker-compose.yml`：

```bash
docker compose up -d
```

### 从 Releases 下载镜像离线部署

```bash
# 下载对应架构的 tar.gz 后导入
docker load -i chinamobile-monitor_1.0.0_linux_amd64.tar.gz

# 运行（ARM 设备把 amd64 换成 arm64）
docker run -d \
  --name chinamobile-monitor \
  -p 10086:10086 \
  -v ./data:/app/data \
  --shm-size 512m \
  -e TZ=Asia/Shanghai \
  --restart unless-stopped \
  chinamobile-monitor:1.0.0-amd64
```

### 源码编译

```bash
go build -trimpath -ldflags="-s -w" -o chinamobile-monitor .
./chinamobile-monitor
```

首次访问 `http://服务器IP:10086` 进入初始化页面创建管理员账号。

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | `10086` | Web 面板端口 |
| `DATA_DIR` | `./data/chinamobile` | 数据目录（账号登录态、设置、日志） |
| `TZ` | `Asia/Shanghai` | 时区 |
| `BROWSER_BIN` | 自动探测 | Chromium 路径（本地运行时指定） |

## 数据与备份

所有数据存储在 `DATA_DIR` 下：

- `store.json` — 账号、设置、每日快照
- `<手机号>/` — 各账号浏览器登录态
- 日志文件

`data/` 目录包含登录态等敏感信息，请妥善备份，切勿泄露。面板内置「设置 → 备份与恢复」可一键导出导入。

## 说明

- 移动数据接口来自官网网页端（wx.10086.cn）；联通来自官网网页端（10010.com，仅用登录态 Cookie 查询本人余量）；
  电信 / 广电接口来自官方客户端协议，仅供个人学习使用
- 移动登录态与设备绑定，跨设备迁移请使用面板内置备份导出 / 导入；联通 / 电信 / 广电为 HTTP 登录态，随备份一起迁移
- 联通查询只需 `JUT` 一个 Cookie（实测 `SHAREJSESSIONID` / `acw_tc` / `piw` 等一概不需要），
  登录态失效时接口返回 `999999`，面板会把账号置为未登录并提示重新登录
- 避免频繁查询对运营商服务器造成压力

## License

MIT
