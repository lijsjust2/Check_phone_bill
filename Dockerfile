# ============================================================
# 四网话费监控（Go 版）
# 构建阶段：编译静态二进制
# 运行阶段：alpine + chromium（移动号登录验证码用；联通 OpenID / 电信 / 广电均为纯 HTTP，无需浏览器）
# ============================================================

# ---- 构建阶段 ----
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/chinamobile-monitor .

# ---- 运行阶段 ----
# chromium : go-rod 无头浏览器本体（BROWSER_BIN 指向 /usr/bin/chromium-browser）
# nss/freetype/harfbuzz : chromium 运行依赖
# su-exec : 启动时修数据目录权限后降权运行
# tzdata  : 时区支持
FROM alpine:3.20
RUN adduser -D -u 1000 app && apk add --no-cache \
    chromium nss freetype harfbuzz ttf-freefont \
    su-exec tzdata ca-certificates
WORKDIR /app
COPY --from=builder /out/chinamobile-monitor /usr/local/bin/chinamobile-monitor
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENV PORT=10086 \
    DATA_DIR=/app/data \
    TZ=Asia/Shanghai \
    BROWSER_BIN=/usr/bin/chromium-browser \
    BROWSER_NO_SANDBOX=1 \
    HOME=/home/app \
    GODEBUG=tlsrsakex=1
VOLUME /app/data
EXPOSE 10086
# 以 root 启动执行 entrypoint（仅用于 chown 数据目录），随后降权为 app 用户
ENTRYPOINT ["/entrypoint.sh"]
