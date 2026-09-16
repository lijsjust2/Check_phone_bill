# 话费监控

四网（移动 / 联通 / 电信 / 广电）话费与套餐用量监控面板（Go 版）。单二进制 + Web 面板，支持多账号管理、定时查询、Bark/PushPlus 推送、2FA 登录保护。

## 功能

### 四网账号管理

- 中国移动：短信验证码登录（内置 Chromium 无头浏览器，网页端完成，无需命令行）
- 中国联通：微信小程序 OpenID 登录（纯 HTTP，无浏览器 / 短信 / 滑块；OpenID 为长期凭证，
  协议同 [Cyborg2017/ha_unicom_bill](https://github.com/Cyborg2017/ha_unicom_bill)：
  `getTicket` 换票 → `serviceEntrance` 换掌厅会话 → 直连 `mxx.client.10010.com` 查询）
- 中国电信：服务密码登录（纯 HTTP 接口，token 长期有效，失效自动重登）
- 中国广电：短信验证码 + 图片验证码登录（浏览器网页端）
- 添加账号时手动选择运营商（携号转网以用户选择为准）
- 账号列表：备注、手机号（脱敏显示，点击查看完整号码）、运营商、状态、余额、通用流量、语音、查询时间
- 单号查询 / 一键查询全部，登录态持久化，一次登录长期使用（联通 OpenID 长期稳定、票据每次现取，电信 token 失效自动密码重登）

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

### 飞牛OS（fnOS）部署

镜像为 linux/amd64 + linux/arm64 双架构，飞牛OS 设备均可使用：

1. 从 GitHub Releases 下载对应架构的镜像 tar.gz（N100 等 x86 主机选 amd64，ARM 盒子选 arm64）
2. 飞牛OS「Docker → 镜像仓库 → 导入镜像」上传 tar.gz（或 SSH 里 `docker load -i`）
3. 「Docker → Compose」新建项目，粘贴：

   ```yaml
   services:
     chinamobile-monitor:
       image: chinamobile-monitor:latest-amd64   # 按导入的镜像标签填写
       container_name: chinamobile-monitor
       restart: unless-stopped
       ports:
         - "10086:10086"
       volumes:
         - /vol1/1000/docker/chinamobile-monitor/data:/app/data  # 换成你的实际路径
       environment:
         - TZ=Asia/Shanghai
       shm_size: 512m
   ```

4. 启动后浏览器访问 `http://飞牛IP:10086` 初始化管理员账号

说明：容器以 root 启动后自动修正数据目录属主并降权到内部 app 用户运行；
移动号登录使用容器内置 Chromium（已配置 `--disable-dev-shm-usage`，未设 `shm_size` 也可运行，建议保留）。

### 联通 OpenID 获取方法

联通号码登录需要微信小程序 OpenID（一次性操作，长期有效）：

1. 电脑端微信打开「中国联通」小程序并登录（没登录过的话先在手机微信登录一次）
2. 用抓包工具（Reqable / Fiddler / Charles，开启 HTTPS 解密并信任证书）拦截 `mina.10010.com` 的请求
3. 在请求体（JSON）中找到 `openId` / `openid` 字段——`o` 开头的 28 位字符串，复制
4. 面板「添加账号 → 中国联通」填入手机号 + OpenID，点击「使用该 OpenID 登录」

注意：必须使用真实登录过联通小程序的微信 OpenID，随机编造的会被拒绝（code 非 0000）。

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

- 移动数据接口来自官网网页端（wx.10086.cn）；联通来自微信小程序接口（mina/mxx.client.10010.com）；
  电信 / 广电接口来自官方客户端协议，仅供个人学习使用
- 移动登录态与设备绑定，跨设备迁移请使用面板内置备份导出 / 导入；联通（OpenID）/ 电信 / 广电为 HTTP 凭证，随备份一起迁移
- 联通 OpenID 是微信侧长期凭证，一般不过期；若查询持续报「OpenID 无效或已失效」，
  说明该微信号需重新在小程序登录一次，再按上文方法重新抓取
- 避免频繁查询对运营商服务器造成压力

## License

MIT
