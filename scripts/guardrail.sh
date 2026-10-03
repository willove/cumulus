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
#                                       # 2026-10-04 起：check 先清学得物再跑，所以它和
#                                       # baseline 一样是**冷跑**、同样花满一遍端点钱
#                                       # （12 题实测 6.7–9.1 万 tokens）。别当廉价探针调。
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
	# 复检必须跑在和基线**同一个学得状态**上。原来这里写着「缓存命中是系统行为的一部分，
	# 两模式同构故可比」——2026-10-04 实测为假：baseline 是 `rm -rf data` 之后跑的（冷），
	# 而它自己那 12 题已经把簇学进了 store，于是 check 是暖的。一次暖复检实测：
	# tokens 91,305→15,327（0.168×）、p90 42,719→14,446ms、ev_rec 7→10，判据照样 pass。
	# 所以先清学得物（语料与向量索引是输入，不是学得物，reset 不碰），并且**要求它真的
	# 清干净**——「调用了 reset」不等于「状态可比」，clean 不为 true 就退，不静默放行。
	rm -f "$STATE/check.jsonl"
	"$ask" -data "$STATE/data" reset learned -yes >/dev/null
	"$ask" -data "$STATE/data" learning | grep -q '"clean": true' || {
		echo "guardrail: learned state still dirty after reset — a warm re-check is not comparable to a cold baseline" >&2
		exit 1
	}
	"$ask" -data "$STATE/data" eval-run -file "$STATE/items.jsonl" -out "$STATE/check.jsonl" \
		-tag guardrail-check -limit "$N" > /dev/null
	"$ask" -data "$STATE/data" calib -guardrail check -rows "$STATE/check.jsonl"
	;;
*)
	echo "guardrail: mode must be baseline|check" >&2; exit 2 ;;
esac
