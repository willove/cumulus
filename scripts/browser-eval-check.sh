#!/usr/bin/env bash
# 浏览器联调门（可选）：一条命令起一个离线 serve，把生产内嵌工作台在真浏览器里
# 走完整流程——题集向导→运行→进度→冻结逐题证据→导出→刷新持久化→对比→深色移动端。
#
# 为什么单列一个门：web/src 的 100 项测试跑的是组合式函数（纯 JS，不起浏览器），
# 渲染与接线（角色名、下载事件、路由、响应式布局）没有回归保护。这个门补上那层，
# 且刻意打**生产 serve 挂的 /ui/**，而不是 vite dev——内嵌 dist 曾经落后于
# web/src，只有打生产包才看得见。
#
# 退出码：0 通过，1 真实失败，2 环境缺席（无 node / 无 Playwright）。
# Playwright 不随本仓安装（见 scripts/browser/playwright.mjs 的说明）。
set -uo pipefail
cd "$(dirname "$0")/.."

command -v node >/dev/null 2>&1 || {
	echo "SKIP browser gate: node is not on PATH (needed to drive the browser check)" >&2
	exit 2
}
# 与 e2e.sh / scenarios/run.sh 一致：依赖 PATH 上的 go，不写死本机路径。
command -v go >/dev/null 2>&1 || {
	echo "SKIP browser gate: go is not on PATH (needed to build the server under test)" >&2
	exit 2
}

WORK="$(mktemp -d)"
PORT=""
SERVE_PID=""
SHOTS="${EVAL_SHOTS:-$(pwd)/var/browser-eval-check}"
cleanup() {
	[ -n "$SERVE_PID" ] && { kill "$SERVE_PID" 2>/dev/null; wait "$SERVE_PID" 2>/dev/null; }
	rm -rf "$WORK"
}
trap cleanup EXIT

# 第一段：运行生命周期 UI 契约（静态托管已构建的 dist + 拦 /v1/**，不起服务端、不建库）。
# 取消/重试/中断/费用确认这些状态在真环境里难以按需复现，用 mock 才能确定性断言；
# 真端到端那一遍在下面。
node scripts/browser/eval-run-states.mjs
STATES=$?
[ "$STATES" -eq 2 ] && exit 2
[ "$STATES" -eq 0 ] || { echo "browser gate: run-state contract failed" >&2; exit 1; }

go build -o "$WORK/cumulus-cluster" ./cmd/cumulus-cluster || { echo "browser gate: build failed" >&2; exit 1; }

# 端口必须是自己占住的：若已有服务在这个端口应答，健康检查会成功，整个门就会
# 静默地跑在别人的库上（并且 kill 掉一个不属于自己的进程）。
# 候选刻意避开 e2e 的 8599–8604：两个门同时跑时不要互相抢端口。
for candidate in ${EVAL_BROWSER_PORT:-8499} 8799 8899 8999; do
	if ! curl -fsS "http://127.0.0.1:$candidate/health" >/dev/null 2>&1; then PORT="$candidate"; break; fi
done
[ -n "$PORT" ] || { echo "browser gate: every candidate port is already serving" >&2; exit 1; }

export CLUS_OFFLINE=1 CLUS_ENV=/dev/null
STORE="$WORK/store"
"$WORK/cumulus-cluster" -data "$STORE" serve -listen "127.0.0.1:$PORT" >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
UP=0
for _ in $(seq 1 100); do
	if ! kill -0 "$SERVE_PID" 2>/dev/null; then
		echo "browser gate: serve exited during startup" >&2; tail -5 "$WORK/serve.log" >&2; exit 1
	fi
	if curl -fsS "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then UP=1; break; fi
	sleep 0.1
done
[ "$UP" = "1" ] || { echo "browser gate: serve did not come up" >&2; tail -5 "$WORK/serve.log" >&2; exit 1; }
[ -d "$STORE" ] || { echo "browser gate: $STORE missing; the gate is not talking to its own store" >&2; exit 1; }

# 夹具就地生成，不依赖 var/ 里的历史文件。
mkdir -p "$WORK/fixtures"
printf '%s\n' '{"id":"q1","query":"缺少参考答案"}' >"$WORK/fixtures/invalid.jsonl"
{
	printf '%s\n' '{"id":"q1","query":"连接池最大连接数是多少？","answer":"128","gold_sources":["manual"]}'
	printf '%s\n' '{"id":"q2","query":"连接超时是多少秒？","answer":"30","gold_sources":["manual"]}'
} >"$WORK/fixtures/valid.jsonl"
# 段落式参考答案：合法但会被规则臂判恒 0，向导应给出警告
python3 - "$WORK/fixtures/passage.jsonl" <<'PY'
import json, sys
answer = '法' * 300  # 超过 eval.MaxShortReference，模拟 LENS 式整段引用
open(sys.argv[1], 'w', encoding='utf-8').write(json.dumps({'id': 'long', 'query': '连接池最大连接数是多少？', 'answer': answer, 'gold_sources': ['manual']}, ensure_ascii=False) + '\n')
PY

EVAL_BASE="http://127.0.0.1:$PORT" \
EVAL_FIXTURE_DIR="$WORK/fixtures" \
EVAL_SHOTS="$SHOTS" \
	node scripts/browser/eval-workbench.mjs
STATUS=$?
[ "$STATUS" -eq 0 ] && echo "browser gate: screenshots in $SHOTS"
exit "$STATUS"