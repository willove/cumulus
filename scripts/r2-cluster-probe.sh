#!/usr/bin/env bash
# R2/R5 probe (design §6.6): cross-system evidence against the Sirchmunk
# reference implementation (ModelScope; arXiv LENS paper's product face).
#
# R2 — cluster-ID split: one question in 10 phrasings; a stable topic identity
#      yields ~1 cluster, sha256(synthesized-text) identity shatters.
# R5 — mis-reuse material: different-topic queries sharing one cluster id
#      (问 A 答 B 串台 candidates) and paraphrase misses (问同答异).
#
# Usage: bash scripts/r2-cluster-probe.sh [sirchmunk_base]
#   default base http://127.0.0.1:8584 ; corpus is a self-contained fixture.
set -u
cd "$(dirname "$0")/.."
BASE="${1:-${SIRCHMUNK_BASE:-http://127.0.0.1:8584}}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Controlled corpus: three disjoint topics in one directory.
mkdir -p "$WORK/corpus"
cat >"$WORK/corpus/pool.md" <<'MD'
# 连接池手册
关键配置：连接池最大 128，超时 30 秒。排队超过 200 直接拒绝。
使用率超过 80% 持续 5 分钟触发告警。
MD
cat >"$WORK/corpus/site.md" <<'MD'
# 部署手册
系统部署在广州机房，端口是 8480。数据库主从采用流复制。
MD
cat >"$WORK/corpus/gw.md" <<'MD'
# 网关说明
服务端口 8484，仅供内网管理面访问。证书每 90 天轮换一次。
MD

SEARCH() { # query mode
	curl -fsS --max-time 120 -X POST "$BASE/api/v1/search" \
		-H 'Content-Type: application/json' \
		-d "{\"query\":$1,\"paths\":\"$WORK/corpus\",\"mode\":\"$2\"}"
}
CLUSTERS() { curl -fsS --max-time 60 "$BASE/api/v1/knowledge/clusters?limit=200"; }

echo "sirchmunk base: $BASE"
curl -fsS --max-time 10 "$BASE/api/v1/knowledge/refresh" >/dev/null 2>&1 || true
BEFORE="$(CLUSTERS | python3 -c 'import json,sys
try: print(len(json.load(sys.stdin).get("clusters") or json.load(sys.stdin).get("data") or []))
except Exception: print(0)')"

PARAPHRASES='[
  "连接池最大连接数是多少",
  "连接池的上限是多少",
  "连接池最多允许多少个连接",
  "连接池size配置",
  "connection pool maximum connections",
  "池子的最大连接数量",
  "连接池最大多少个",
  "连接数量上限怎么配",
  "连接池容量一般设多少",
  "连接池允许的最大并发连接数"
]'

echo "== R2: 10 phrasings, one topic =="
python3 - "$PARAPHRASES" >"$WORK/r2.json" <<'PY'
import json, sys
phr = json.loads(sys.argv[1])
for p in phr:
    print(json.dumps({"q": p, "expect": "pool"}))
PY
# Send the paraphrases (FAST mode; each creates/evolves clusters server-side).
N=0
while IFS= read -r line; do
	Q="$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["q"]))')"
	R="$(SEARCH "$Q" FAST | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin); print(d.get("answer") or d.get("summary") or "")[:60]
except Exception as e: print("")')"
	N=$((N+1))
	printf '  [%02d] %s\n' "$N" "$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["q"])')"
done < "$WORK/r2.json"

curl -fsS --max-time 30 -X POST "$BASE/api/v1/knowledge/refresh" >/dev/null 2>&1 || true
python3 - "$BEFORE" <<'PY'
import json, os, subprocess, sys, urllib.request

base = os.environ.get("BASE", "http://127.0.0.1:8584")
def get(path):
    with urllib.request.urlopen(base + path, timeout=60) as r:
        return json.load(r)
before = int(sys.argv[1] or 0)
d = get("/api/v1/knowledge/clusters?limit=200")
clusters = d.get("clusters") or d.get("data") or []
pool_marks = ("连接池", "connection pool", "池")
family = []
for c in clusters:
    qs = " ".join(c.get("queries") or [])
    if any(m in qs.lower() or m in qs for m in pool_marks):
        family.append({"id": c.get("id"), "n_queries": len(c.get("queries") or []),
                      "sample": (c.get("queries") or [""])[:3]})
print("R2 clusters total=%d (before=%d) · pool-family=%d" % (len(clusters), before, len(family)))
for f in family:
    print("   id=%s queries=%d %s" % (f["id"], f["n_queries"], f["sample"]))
print("R2 verdict: %s" % ("STABLE (==1 cluster)" if len(family) == 1 else
                          "SPLIT (%d clusters for one topic)" % len(family)))
PY

echo "== R5: cross-topic mis-reuse scan =="
python3 - <<'PY'
import json, os, urllib.request

base = os.environ.get("BASE", "http://127.0.0.1:8584")
with urllib.request.urlopen(base + "/api/v1/knowledge/clusters?limit=200", timeout=60) as r:
    d = json.load(r)
clusters = d.get("clusters") or d.get("data") or []

topics = {
    "pool": ("连接池", "connection pool", "并发", "排队"),
    "site": ("机房", "广州", "主从", "数据库"),
    "gw":   ("端口", "8484", "证书", "内网"),
}
def topic_of(text):
    t = (text or "").lower()
    hits = {name: sum(m.lower() in t for m in marks) for name, marks in topics.items()}
    best = max(hits, key=lambda k: hits[k])
    return best if hits[best] > 0 else "other"

mixed = []
for c in clusters:
    qs = c.get("queries") or []
    ts = {topic_of(q) for q in qs}
    if len(ts - {"other"}) > 1:
        mixed.append((c.get("id"), sorted(ts), len(qs)))
if mixed:
    print("R5 candidates (one cluster serving different topics — 问 A 答 B material):")
    for cid, ts, n in mixed:
        print("   id=%s topics=%s queries=%d" % (cid, ts, n))
else:
    print("R5: no cross-topic cluster mixing found in %d clusters" % len(clusters))
PY
echo "probe done"
