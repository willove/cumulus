#!/usr/bin/env bash
# θ 探针净室（ir-rag / MeanCache-SCALM 阈值搜索，embed-notes §10-1）。
# 目的：测 L2 复用线在**当前 embedder 空间**里的落点。标签用**来源**：
# 查询文本取自哪篇法条，正确簇就是锚在该法条上的簇（expect_key）。这不是
# golden 答案集——查询原文即法条，正确答案由构造定义；探针不看判官、不
# 看评测题，也**不选 θ**（只出分布+网格）。
#
# 为什么用净室：复用分是「查询 embed × 簇 embed」的同空间量，混两种
# embedder 建的簇会让余弦不可比。故用一次性 store 目录，单一 embedder 端到端。
#
# 用法：
#   CLUS_EMBED=minilm scripts/theta-probe.sh        # 全自动（var/thetaprobe/data）
#   N=8 REUSE=1 scripts/theta-probe.sh             # 可调 / 复用既有 store
# 产物：var/thetaprobe/{seeds.jsonl,build.txt,probe.json}；只报告不写回配置。
set -euo pipefail
cd "$(dirname "$0")/.."
N="${N:-6}"
STATE="$(pwd)/var/thetaprobe"
STORE="$STATE/data"
SRCDIR="${LAW_DIR:-$HOME/datasets/KuugoRen/Chinese_Law}"
mkdir -p "$STATE"

echo "theta-probe: store=$STORE seed=$N embedder=${CLUS_EMBED:-<local-hash default>}"

# 1) 全量法条 → 取样语料；建簇用原文，探针问句由 anchorgen 从同一条文生成
#    口语问（Self-Index Query Simulator 的生成侧，非 golden；标签=来源条文）。
python3 scripts/chinalaw_corpus.py --dir "$SRCDIR" --out "$STATE/corpus_all.jsonl"
python3 scripts/sample_articles.py "$STATE/corpus_all.jsonl" "$STATE/corpus.jsonl" "$N"
export ANCHOR_KEY="${ANCHOR_KEY:-$(grep -E '^LLM_API_KEY=' .env | cut -d= -f2)}"
go build -o "$STATE/anchorgen" ./cmd/anchorgen
"$STATE/anchorgen" -n "$N" < "$STATE/corpus.jsonl" > "$STATE/seeds.jsonl"

# 2) 净室库：默认丢弃上一次的 store 目录（旧库 + 已耗尽的 job 游标会让新语料
#    静默不入库——实测踩过）。REUSE=1 才复用。
if [ "${REUSE:-0}" = "1" ]; then
	echo "theta-probe: REUSE=1 — 复用既有 store $STORE" >&2
else
	rm -rf "$STORE"
fi
go build -o "$STATE/cumulus-cluster" ./cmd/cumulus-cluster
"$STATE/cumulus-cluster" -data "$STORE" ensure >/dev/null
"$STATE/cumulus-cluster" -data "$STORE" ingest-jsonl -file "$STATE/corpus.jsonl" -job theta >/dev/null
echo "theta-probe: ingested $STORE ($(wc -l <"$STATE/corpus.jsonl") articles)"

# 3) 建簇：用原文问句（anchorgen 的口语问句留给探针当未见样本）
python3 scripts/texts_only.py "$STATE/corpus.jsonl" > "$STATE/build.txt"
while IFS= read -r q; do
	[ -n "$q" ] || continue
	"$STATE/cumulus-cluster" -data "$STORE" search -q "$q" -raw >/dev/null 2>&1 || true
done < "$STATE/build.txt"
CLUSTERS=$("$STATE/cumulus-cluster" -data "$STORE" cluster list | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))')
echo "theta-probe: build_queries=$(wc -l <"$STATE/build.txt") clusters=$CLUSTERS"
if [ "$CLUSTERS" -lt 2 ]; then
	echo "theta-probe: <2 clusters — 提高 N" >&2
	exit 1
fi

# 4) 探针：argmax 簇的锚定法条 vs expect_key；分布 + θ 网格（不设线）
go build -o "$STATE/thetaprobe" ./cmd/thetaprobe
CLUS_EMBED="${CLUS_EMBED:-}" "$STATE/thetaprobe" -lite "$STORE" -seeds "$STATE/seeds.jsonl" | tee "$STATE/probe.json"
echo "theta-probe: 口语问句样例：" >&2
head -3 "$STATE/seeds.jsonl" | python3 scripts/show_queries.py >&2
