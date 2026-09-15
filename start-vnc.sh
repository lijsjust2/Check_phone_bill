#!/bin/sh
# 虚拟显示 + noVNC 栈：用于 Docker/NAS 无头环境下的联通滑块登录
# 由 entrypoint 在 VNC_ENABLED=1 时以 app 用户身份后台启动。
# 联通登录浏览器渲染到 Xvnc 虚拟显示(:99)，用户通过 noVNC 网页远程操作滑块/短信。

DISP="${DISPLAY:-:99}"
DNUM="${DISP#:}"                 # :99 -> 99
DPORT=$((5900 + DNUM))           # VNC TCP 端口 = 5900 + 显示号
VPORT="${VNC_PORT:-6080}"
GEOM="${VNC_GEOMETRY:-1280x800}"
DEPTH="${VNC_DEPTH:-24}"

echo "[VNC] 启动虚拟显示 ${DISP} (${GEOM}x${DEPTH}) ..."
# Xvnc 既是 X server 又是 VNC server；-localhost 仅本机可连，由 websockify 转发到 6080
Xvnc "${DISP}" -geometry "${GEOM}" -depth "${DEPTH}" \
  -SecurityTypes None -localhost -nolisten tcp >/tmp/xvnc.log 2>&1 &

# 等待 X socket 就绪
for _ in $(seq 1 30); do
  [ -e "/tmp/.X11-unix/X${DNUM}" ] && break
  sleep 0.5
done

echo "[VNC] 启动 noVNC(websockify) :${VPORT} -> localhost:${DPORT}"
websockify --web=/usr/share/novnc "${VPORT}" "localhost:${DPORT}" >/tmp/websockify.log 2>&1 &

echo "[VNC] 就绪：浏览器打开 http://<本机IP>:${VPORT}/vnc.html 即可操作联通登录窗口"
wait
