#!/usr/bin/env bash
# 启动 Janus。
#
# 配置来源：janus.env（同目录）；不存在则用内置默认值。
# 真实环境变量优先级更高，可临时覆盖，例如：
#     BRIDGE_LOG_LEVEL=debug ./run.sh
set -euo pipefail

cd "$(dirname "$0")/.."

export PATH="${PATH}:/usr/local/go/bin"

if [ ! -f janus.env ]; then
  if [ -f janus.env.example ]; then
    echo "[config] 未找到 janus.env，从模板复制一份（请按需修改）"
    cp janus.env.example janus.env
  fi
fi

if [ ! -x ./janus ]; then
  echo "[build] 编译 bridge…"
  go build -o janus .
fi

# 绑定非回环地址又没设 key 时给个醒目提示
ADDR="$(sed -n 's/^BRIDGE_ADDR=//p' janus.env 2>/dev/null | head -1)"
ADDR="${BRIDGE_ADDR:-${ADDR:-0.0.0.0:2810}}"
KEY="${BRIDGE_API_KEY:-$(sed -n 's/^BRIDGE_API_KEY=//p' janus.env 2>/dev/null | head -1)}"
case "$ADDR" in
  127.0.0.1:*|localhost:*|"[::1]:"*) ;;
  *) [ -z "$KEY" ] && echo "⚠️  监听 $ADDR 但 BRIDGE_API_KEY 为空 —— 任何能访问该端口的人都能消耗你的额度" ;;
esac

exec ./janus
