#!/usr/bin/env bash
# scale-eval —— 真实规模站起来（9,600 法条全量语料 + 真实口语问）。
#
# 与所有此前的 30-40 篇玩具语料判决不同：这是把系统放进它该服务的真实
# 规模——摄取全量、真实分布问句、ev_rec 以 qrels 判定（judge-free）。
# 产物是第一个真实规模的基线数字，任何后续优化以此为准绳。
#
# cn-law 语料的字段是 id/body（不是我们的 key/text）——不转换则正文全空
# 入库（实测：60/60 题 DEEP 零证据）。转换是显式的：真实异构语料先归一
# 再进管线。
#
# 用法：scripts/scale-eval.sh [N]      # 默认 60 问
set -eu
cd "$(dirname "$0")/.."
N="${1:-60}"
SRC="$HOME/datasets/cn-law-rag/case"
STATE="$(pwd)/var/scale-eval"
ask="$STATE/cumulus-cluster"
mkdir -p "$STATE"
go build -o "$ask" ./cmd/cumulus-cluster
export CLUS_EMBED=minilm

if [ ! -s "$STATE/corpus.norm.jsonl" ]; then
	python3 - "$SRC/corpus.jsonl" "$STATE/corpus.norm.jsonl" <<'PY'
import json, sys
src, out = sys.argv[1], sys.argv[2]
n = 0
with open(out, "w", encoding="utf-8") as f:
    for line in open(src, encoding="utf-8"):
        r = json.loads(line)
        f.write(json.dumps({"key": r.get("id") or r.get("key"),
                            "title": r.get("title") or r.get("id"),
                            "text": r.get("body") or r.get("text")},
                           ensure_ascii=False))
        f.write("\n")
        n += 1
print("normalized %d docs" % n)
PY
fi

if [ ! -d "$STATE/data" ]; then
	"$ask" -data "$STATE/data" ensure >/dev/null
	"$ask" -data "$STATE/data" ingest-jsonl -file "$STATE/corpus.norm.jsonl" -job scale
	"$ask" -data "$STATE/data" ensure -embed
	echo "scale: corpus ingested"
fi

python3 - "$SRC/queries.jsonl" "$STATE/items.jsonl" "$N" <<'PY'
import json, sys, hashlib
src, out, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
rows = []
for line in open(src, encoding="utf-8"):
    r = json.loads(line)
    q = r.get("query", "").strip()
    rel = [x for x in (r.get("relevant") or []) if x]
    if not q or not rel:
        continue
    rows.append({"id": r["id"], "query": q, "relevant": rel})
rows.sort(key=lambda r: hashlib.sha256(r["id"].encode()).hexdigest())
picked = rows[:n]
with open(out, "w", encoding="utf-8") as f:
    for r in picked:
        f.write(json.dumps({"id": r["id"], "query": r["query"],
                            "answer": "(gold: " + r["id"] + ")",
                            "gold_sources": r["relevant"]},
                           ensure_ascii=False))
        f.write("\n")
print("scale: %d items written" % len(picked))
PY

"$ask" -data "$STATE/data" eval-run \
	-file "$STATE/items.jsonl" -out "$STATE/results.jsonl" \
	-tag "scale-$N" -limit "$N" > "$STATE/report.json" 2>&1 || true

python3 - "$STATE" <<'PY'
import json, sys, collections
state = sys.argv[1]
rows = [json.loads(l) for l in open(state + "/results.jsonl", encoding="utf-8")]
n = len(rows)
if n == 0:
    print("no rows"); sys.exit(1)
ev = sum(1 for r in rows if (r.get("eval") or {}).get("ev_rec"))
modes = collections.Counter(r.get("mode") for r in rows)
lat = sorted(r.get("latency_ms") or 0 for r in rows)
tok = sum(r.get("search_tokens") or 0 for r in rows)
print("n=%d ev_rec=%d/%d (%.1f%%)" % (n, ev, n, 100 * ev / n))
print("modes:", dict(modes))
print("lat p50=%.1fs p90=%.1fs" % (lat[n // 2] / 1000, lat[9 * n // 10] / 1000))
print("total_search_tokens=%d mean=%d" % (tok, tok // n))
PY
