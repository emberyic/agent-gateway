#!/bin/bash
# 网关验证脚本：普通转发 / SSE 零缓冲 / IP 限流 / 流式并发闸
#
# 用法：
#   终端 1: go run test_backend.go
#   终端 2: go run ./cmd/gateway -config configs/gateway.test.yml
#   终端 3: bash scripts/benchmark.sh
#
# 注意：脚本里的「并发闸」一节依赖 configs/gateway.test.yml 的
# max_concurrent_streams: 2 和 rate_limit 突发 3。换配置请同步调整下面的期望值

set -u

GATEWAY=${GATEWAY:-http://localhost:8080}
PASS=0
FAIL=0

ok()   { echo " $1"; PASS=$((PASS + 1)); }
bad()  { echo " $1"; FAIL=$((FAIL + 1)); }
check() { # check <描述> <实际> <期望>
    if [ "$2" = "$3" ]; then ok "$1（$2）"; else bad "$1：期望 $3，实际 $2"; fi
}

echo "========================================"
echo " my_gateway 验证  ->  $GATEWAY"
echo "========================================"

echo
echo "1. 网关自身端点（不参与限流）"
check "/healthz 状态码" "$(curl -s -o /dev/null -w '%{http_code}' "$GATEWAY/healthz")" "200"
curl -s "$GATEWAY/healthz"; echo
if curl -s "$GATEWAY/metrics" | grep -q 'gateway_requests_total'; then
    ok "/metrics 输出 Prometheus 指标"
else
    bad "/metrics 没有指标输出"
fi

echo
echo "2. 普通转发（/api/test）"
code=$(curl -s -o /dev/null -w '%{http_code}' "$GATEWAY/api/test")
check "状态码" "$code" "200"
check "响应头 X-Gateway-Route" "$(curl -s -D- -o /dev/null "$GATEWAY/api/test" | tr -d '\r' | awk -F': ' '/^X-Gateway-Route/{print $2}')" "/api"

echo
echo "3. SSE 流式透传（关键：必须逐帧到达，不能被网关攒批）"
echo "   query 必须原样到达后端，下面用 question/session_id 回显验证："
curl -sN --max-time 10 "$GATEWAY/chat/stream?question=hello-world&session_id=bench_001&frames=3" |
    while IFS= read -r line; do
        [ -n "$line" ] && printf '   [%s] %s\n' "$(date +%H:%M:%S.%3N)" "$line"
    done
echo "   ↑ 每行前面是到达时间：帧之间应相差约 0.3 秒，且第一行能看到 question/session_id 回显。"
echo "     如果所有帧时间几乎相同、或只在最后一次性出现，说明网关把它缓冲了。"

echo
echo "4. IP 限流（configs/gateway.test.yml: rps 5 / 突发 3）"
codes=""
for _ in $(seq 1 8); do
    codes="$codes $(curl -s -o /dev/null -w '%{http_code}' "$GATEWAY/api/test")"
done
echo "   8 次连发的状态码:$codes"
if echo "$codes" | grep -q 429; then ok "出现 429，限流生效"; else bad "没有出现 429"; fi
retry_after=$(curl -s -D- -o /dev/null "$GATEWAY/api/test" | tr -d '\r' | awk -F': ' '/^Retry-After/{print $2}')
if [ -n "$retry_after" ]; then ok "429 带 Retry-After=$retry_after"; else echo "  ℹ️  本次请求未被限流（桶里有令牌），跳过 Retry-After 检查"; fi

echo
echo "5. 流式并发闸（max_concurrent_streams: 2）"
echo "   先开 2 条慢速流（/chat/slow 每帧 1 秒，共 10 帧）占满槽位..."
curl -sN --max-time 15 "$GATEWAY/chat/slow" >/dev/null &
p1=$!
curl -sN --max-time 15 "$GATEWAY/chat/slow" >/dev/null &
p2=$!
sleep 1.5
body=$(curl -s -w '\n%{http_code}' --max-time 5 "$GATEWAY/chat/slow")
code=$(echo "$body" | tail -n1)
check "第 3 条流的并发闸状态码" "$code" "429"
if echo "$body" | grep -q 'too_many_streams'; then
    ok "错误码是 too_many_streams（不是 IP 限流，区分得开）"
else
    bad "期望错误码 too_many_streams，实际：$(echo "$body" | head -n1)"
fi
echo "   等待占位的两条流结束..."
wait $p1 $p2 2>/dev/null
sleep 1
check "槽位释放后新流可以正常建立" "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$GATEWAY/chat/stream?frames=1")" "200"

echo
echo "6. 压测（可选，需要 wrk）"
if command -v wrk >/dev/null 2>&1; then
    # 限流会挡掉大部分请求，压测时建议临时提高 rate_limit 或加 -H 换 IP
    wrk -t4 -c100 -d15s --latency "$GATEWAY/api/test"
else
    echo "未安装 wrk，跳过。Ubuntu: sudo apt-get install wrk / Mac: brew install wrk"
fi

echo
echo "========================================"
echo " 通过 $PASS 项，失败 $FAIL 项"
echo "========================================"
[ "$FAIL" -eq 0 ]
