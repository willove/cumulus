#!/usr/bin/env bash
# guardrail —— 金题护栏（纯报警器，不是老师）。
#
# 纪律（写死在用途里）：本脚本永不提供学习信号，只行使否决——任何被 calib/learn
# 应用的变更若让护栏掉线，调用方必须回滚。指标只用 judge-free 量：ev_rec 是引用
# 集与 gold 的纯集合成员判定（internal/eval Score，无 LLM）、answered、tokens、
# latency_ms。向金题过拟合在结构上不可能：你无法向一个只说"不行"的东西做梯度。
# 比较逻辑在 internal/calib（Go，hermetic 测试钉边界），本脚本只做编排。
#
# 用法：
#   scripts/guardrail.sh baseline [N]   # 建立基线（新库跑 N 题冻结切片）
#   scripts/guardrail.sh check   [N]    # 复检对照基线，回归则非零退出
#   CLUS_SELFPLAY_RATE=0 强制关——护栏测量本身绝不吃采样预算。
set -eu
cd "$(dirname "$0")/.."
MODE="${1:-check}"
N="${2:-12}"
STATE="$(pwd)/var/guardrail"
SRC="$(pwd)/var/semhead-ab"          # 冻结集（题切片取前 N）
ask="$STATE/cumulus-cluster"
mkdir -p "$STATE"
go build -o "$ask" ./cmd/cumulus-cluster
export CLUS_EMBED=minilm CLUS_SELFPLAY_RATE=0

case "$MODE" in
baseline)
	rm -rf "$STATE/data" "$STATE/results.jsonl"
	"$ask" -data "$STATE/data" ensure >/dev/null
	"$ask" -data "$STATE/data" ingest-jsonl -file "$SRC/corpus.jsonl" -job guardrail >/dev/null
	"$ask" -data "$STATE/data" ensure -embed >/dev/null
	head -n "$N" "$SRC/items.jsonl" > "$STATE/items.jsonl"
	"$ask" -data "$STATE/data" eval-run -file "$STATE/items.jsonl" -out "$STATE/results.jsonl" \
		-tag guardrail-baseline -limit "$N" > /dev/null
	"$ask" -data "$STATE/data" calib -guardrail baseline -rows "$STATE/results.jsonl"
	;;
check)
	[ -s "$STATE/results.jsonl" ] || { echo "guardrail: no baseline (run: guardrail.sh baseline)" >&2; exit 2; }
	# 复检同库重跑：缓存命中是系统行为的一部分，两模式同构故可比。
	rm -f "$STATE/check.jsonl"
	"$ask" -data "$STATE/data" eval-run -file "$STATE/items.jsonl" -out "$STATE/check.jsonl" \
		-tag guardrail-check -limit "$N" > /dev/null
	"$ask" -data "$STATE/data" calib -guardrail check -rows "$STATE/check.jsonl"
	;;
*)
	echo "guardrail: mode must be baseline|check" >&2; exit 2 ;;
esac
