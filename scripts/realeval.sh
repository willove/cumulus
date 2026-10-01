#!/usr/bin/env bash
# Real-corpus LENS 式评测（R-E1，lens-notes §7.4）。
# cn-law-rag（Apache-2.0）：锚点口语问 + 正例法条 + 难负法条。正/负法条全部
# 入语料（难负例是评测点），锚点过真实管线（FAST→DEEP），Closed-Book 直答
# 对照，端点判官（judge_correct 资产）判 Correct；聚合 EM/Ev.Rec/Ground +
# 失败四分类 + McNemar（cumulus-cluster vs closed-book）。断点续跑：结果逐项落盘。
# 端点配置走套件 ./.env（LLM_* canonical，旧 AIGATE_*/LLM_MODEL_NAME 别名兼容）；本脚本不隔离 .env。
# 用法：
#   scripts/realeval.sh prep [N]   # 取样+建库+摄取（默认 30 锚点，确定性取样）
#   LIMIT=4 scripts/realeval.sh step   # 跑一小批（默认 4 项）
#   scripts/realeval.sh report     # 全量聚合报告
set -u
cd "$(dirname "$0")/.."
DATA="${CNLAW_DIR:-$HOME/datasets/cn-law-rag/finetune_dataset.jsonl}"
N="${REALEVAL_N:-30}"
STATE="$(pwd)/var/realeval"
STORE="$STATE/data"
mkdir -p "$STATE"

# One embedded store directory — no server process, no port. Badger locks the
# directory, so nothing else may hold it while a phase runs.
build_ask() { go build -o "$STATE/cumulus-cluster" ./cmd/cumulus-cluster || exit 1; }

case "${1:-report}" in
prep)
	[ -f "$DATA" ] || { echo "realeval: $DATA not found" >&2; exit 1; }
	build_ask
	python3 - "$DATA" "$N" "$STATE" <<'PY'
import json, sys
src, n, state = sys.argv[1], int(sys.argv[2]), sys.argv[3]
items, docs, pos2key = [], {}, {}
with open(src, encoding="utf-8") as f:
    for line in f:
        if len(items) >= n:
            break
        try:
            t = json.loads(line)
        except Exception:
            continue
        a, pos, neg = t.get("anchor"), t.get("positive"), t.get("negative")
        if not (a and pos and neg):
            continue
        k = len(items)
        key = pos2key.get(pos)
        if key is None:
            key = "law%03d" % k
            pos2key[pos] = key
            docs[key] = pos
        docs["neg%03d" % k] = neg
        items.append({"id": "q%03d" % k, "query": a,
                      "answer": pos, "gold_sources": [key]})
with open(state + "/items.jsonl", "w", encoding="utf-8") as f:
    for it in items:
        f.write(json.dumps(it, ensure_ascii=False) + "\n")
with open(state + "/corpus.jsonl", "w", encoding="utf-8") as f:
    for key, text in docs.items():
        f.write(json.dumps({"key": key, "title": key, "text": text}, ensure_ascii=False) + "\n")
print("items=%d docs=%d" % (len(items), len(docs)))
PY
	"$STATE/cumulus-cluster" -data "$STORE" ensure
	"$STATE/cumulus-cluster" -data "$STORE" ingest-jsonl -file "$STATE/corpus.jsonl" -job realeval
	;;
step)
	build_ask
	OUT="$STATE/results.jsonl"
	EXTRA=""
	if [ "${L1PRE:-0}" = "1" ]; then
		OUT="$STATE/results_l1.jsonl"   # 对照组：body_embed KNN 收窄候选（CLUS_EMBED=minilm）
		EXTRA="-l1pre"
	fi
	"$STATE/cumulus-cluster" -data "$STORE" eval-run -file "$STATE/items.jsonl" \
		-out "$OUT" -judge -prior $EXTRA -limit "${LIMIT:-4}"
	;;
report)
	build_ask
	# All items already recorded → resume-only pass; the aggregate is printed.
	# The flags MUST mirror step's: the aggregate is stamped with THIS
	# invocation's config fingerprint, so aggregating judge+prior rows under
	# a no-judge/no-prior invocation mislabels the scorecard — the report
	# once read judged:false / prior:false on judge-flipped numbers.
	OUT="$STATE/results.jsonl"
	EXTRA=""
	if [ "${L1PRE:-0}" = "1" ]; then
		OUT="$STATE/results_l1.jsonl"   # 对照组：body_embed KNN 收窄候选（CLUS_EMBED=minilm）
		EXTRA="-l1pre"
	fi
	"$STATE/cumulus-cluster" -data "$STORE" eval-run -file "$STATE/items.jsonl" \
		-out "$OUT" -judge -prior $EXTRA -limit 0
	;;
esac
