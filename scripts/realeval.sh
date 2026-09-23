#!/usr/bin/env bash
# Real-corpus LENS 式评测（R-E1，lens-notes §7.4）。
# cn-law-rag（Apache-2.0）：锚点口语问 + 正例法条 + 难负法条。正/负法条全部
# 入语料（难负例是评测点），锚点过真实管线（FAST→DEEP），Closed-Book 直答
# 对照，端点判官（judge_correct 资产）判 Correct；聚合 EM/Ev.Rec/Ground +
# 失败四分类 + McNemar（ask vs closed-book）。断点续跑：结果逐项落盘。
# 端点配置走套件 ./.env（LLM_* → AIGATE_*）；本脚本不隔离 .env。
# 用法：
#   scripts/realeval.sh prep [N]   # 取样+建库+摄取（默认 30 锚点，确定性取样）
#   LIMIT=4 scripts/realeval.sh step   # 跑一小批（默认 4 项）
#   scripts/realeval.sh report     # 全量聚合报告
set -u
cd "$(dirname "$0")/.."
DATA="${CNLAW_DIR:-$HOME/datasets/cn-law-rag/finetune_dataset.jsonl}"
N="${REALEVAL_N:-30}"
DB_PORT="${REALEVAL_PORT:-8593}"
STATE="$(pwd)/var/realeval"
mkdir -p "$STATE"
SRV="http://127.0.0.1:$DB_PORT"

db_up() {
	if curl -fsS "$SRV/v1/health" >/dev/null 2>&1; then return 0; fi
	(cd ../db-works/cumudb && go build -o "$STATE/cumudb" ./cmd/cumudb) || exit 1
	go build -o "$STATE/ask" ./cmd/ask || exit 1
	"$STATE/cumudb" -listen "127.0.0.1:$DB_PORT" -data "$STATE/data" -log-level warn >>"$STATE/cumudb.log" 2>&1 &
	for _ in $(seq 1 50); do
		curl -fsS "$SRV/v1/health" >/dev/null 2>&1 && return 0
		sleep 0.2
	done
	echo "realeval: cumudb failed to start (see $STATE/cumudb.log)" >&2
	exit 1
}

case "${1:-report}" in
prep)
	[ -f "$DATA" ] || { echo "realeval: $DATA not found" >&2; exit 1; }
	db_up
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
	"$STATE/ask" -server "$SRV" ensure
	"$STATE/ask" -server "$SRV" ingest-jsonl -file "$STATE/corpus.jsonl" -job realeval
	;;
step)
	db_up
	OUT="$STATE/results.jsonl"
	EXTRA=""
	if [ "${L1PRE:-0}" = "1" ]; then
		OUT="$STATE/results_l1.jsonl"   # 对照组：body_embed KNN 收窄候选（ASK_EMBED=minilm）
		EXTRA="-l1pre"
	fi
	"$STATE/ask" -server "$SRV" eval-run -file "$STATE/items.jsonl" \
		-out "$OUT" -judge -prior $EXTRA -limit "${LIMIT:-4}"
	;;
report)
	db_up
	# All items already recorded → resume-only pass; the aggregate is printed.
	OUT="$STATE/results.jsonl"
	if [ "${L1PRE:-0}" = "1" ]; then OUT="$STATE/results_l1.jsonl"; fi
	"$STATE/ask" -server "$SRV" eval-run -file "$STATE/items.jsonl" \
		-out "$OUT" -limit 0
	;;
esac
