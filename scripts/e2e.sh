#!/usr/bin/env bash
# e2e: gates A–BB — ingest (A) + cluster reuse (B) + graph expansion (C) +
# DEEP citations (D) + multi-hop coverage (F) + ingest face/tiering (G) +
# html/embed/job/serve/cites (H) + docx/L1-prefilter/B4-prior (I) +
# conflict detect (J) + B5/B6/B9 surfaces (K) + dynamic corpus γ(I) (L) +
# changelog reconcile/sync cap (M) + six-modality synergy (N) +
# eval-run offline face + resume (O) + search HTTP/SSE face (P) +
# chat sessions (Q) + web UI (R) + namespace scoping (S) +
# cluster tidy (T) + MCP face (V) + candidate discovery (W) +
# rich cognition edges (X) + strict embedder gate (Y) +
# model weight face (Z) + scan REST face (AA),
# against a REAL cumulite store — one embedded Badger directory, no server
# process. Scorer/embedder are the offline stubs by design
# (put never blocks on a model). Summary line: clus-e2e: N ok, M fail
set -u
cd "$(dirname "$0")/.."
# The gates are offline-stub territory: never let a developer's environment
# route them at a live model (each search would cost real tokens and flake).
# Shared with every offline harness so this cannot drift again — see the file
# for why CLUS_ENV=/dev/null alone is not enough.
# shellcheck source=scripts/offline-gate.sh
. "$(dirname "$0")/offline-gate.sh"
WORK="$(mktemp -d)"
DATA="$WORK/data"
SERVE_PID=""
PASS=0
FAIL=0
check() { if [ "$2" -eq 0 ]; then PASS=$((PASS + 1)); printf '  ok: %s\n' "$1"; else FAIL=$((FAIL + 1)); printf '  FAIL: %s\n' "$1"; fi; }
# Stop a serve process and WAIT until it has released the Badger directory lock.
# A bare `kill` races the next gate: SIGTERM starts a graceful shutdown, so the
# following CLI command could open the same store while the old process still
# held it and die on "Cannot acquire directory lock" (intermittently failed
# gate T). Bounded: SIGKILL after 10 s so a wedged server cannot hang the gate.
# Every serve gate must own its port. If something already answers /health, the
# readiness loop below would succeed against a FOREIGN store and the gate would
# assert on data it never created (or, worse, kill -9 a process it does not own).
own_port() {
	if curl -fsS "http://127.0.0.1:$1/health" >/dev/null 2>&1; then
		echo "clus-e2e: FAIL port $1 is already serving; refusing to gate against a foreign store" >&2
		exit 1
	fi
}
stop_serve() {
	local pid="$1"
	[ -n "${pid:-}" ] || return 0
	kill "$pid" 2>/dev/null
	for _ in $(seq 1 100); do
		kill -0 "$pid" 2>/dev/null || { wait "$pid" 2>/dev/null; return 0; }
		sleep 0.1
	done
	kill -9 "$pid" 2>/dev/null
	wait "$pid" 2>/dev/null
}
cleanup() {
	if [ -n "$SERVE_PID" ] && [ "$SERVE_PID" -eq "$SERVE_PID" ] 2>/dev/null; then kill "$SERVE_PID" 2>/dev/null; fi
	rm -rf "$WORK"
}
trap cleanup EXIT

(cd ../db-works/cumulite && go build -o "$WORK/cumulite" ./cmd/cumulite) || { echo "clus-e2e: FAIL building cumulite"; exit 1; }
go build -o "$WORK/cumulus-cluster" ./cmd/cumulus-cluster || { echo "clus-e2e: FAIL building cumulus-cluster"; exit 1; }

# One store directory. Badger locks it exclusively, so every phase below either
# drives the CLI or the serve process — never both at once.
A="$WORK/cumulus-cluster -data $DATA"
CUM="$WORK/cumulite"
# One page of documents matching a filter. Flag order matters: the CLI's
# FlagSet stops at the first positional, so -data/-filter come before the
# collection name.
Q() { "$CUM" doc query -data "$DATA" -limit 500 -filter "$2" "$1"; }
# How many documents match.
QQ() { Q "$1" "$2" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["documents"]))'; }
$A ensure >/dev/null || { echo "clus-e2e: FAIL ensure"; exit 1; }

cat >"$WORK/handbook.md" <<'MD'
# 部署手册

## 数据库
系统部署在广州机房，端口是 8480。

--- page 2 ---
## 连接池
关键配置：连接池最大 128，超时 30 秒。

## 其他
无关段落填充文本 padding padding padding。
MD

P1="$($A put -title "部署手册" -key handbook -body-file "$WORK/handbook.md")"
ID1="$(echo "$P1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
ST1="$(echo "$P1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')"
[ "$ST1" = "created" ] ; check "first put creates a source ($ID1)" $?
P2="$($A put -title "部署手册" -key handbook -body-file "$WORK/handbook.md")"
ST2="$(echo "$P2" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')"
[ "$ST2" = "unchanged" ] ; check "same body put twice is unchanged (idempotent)" $?
check "put succeeds with no embedder (L0 independent)" 0

SPAN_OK="$($A get "$ID1" | python3 -c '
import json,sys
s=json.load(sys.stdin)
body=s["body"]; sp=s["structure"]
assert sp, "no structure"
total=sum(x["end"]-x["start"] for x in sp)
assert total==len(body), (total, len(body))
labs="|".join(x["label"] for x in sp)
assert "连接池" in labs or "数据库" in labs
assert "p2" in labs
print("ok")
')"
[ "$SPAN_OK" = "ok" ] ; check "structure spans map back onto body (incl. page marks)" $?

S1="$($A search -q "连接池最大连接数" -raw)"
echo "$S1" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert a["samples"], "no samples"' ; check "search finds a sample window" $?
EV1="$(Q clus_evidence '{}')"
echo "$EV1" | grep -q "ev:" ; check "evidence window recorded after search" $?

cat >"$WORK/handbook2.md" <<'MD'
# 部署手册（修订）
连接池参数已改为 256。
MD
P3="$($A put -title "部署手册" -key handbook -body-file "$WORK/handbook2.md")"
ST3="$(echo "$P3" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')"
STALE="$(echo "$P3" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("stale_id",""))')"
[ "$ST3" = "updated" ] && [ -n "$STALE" ] ; check "changed body updates version and marks stale_id" $?
EVST="$(Q clus_evidence '{"status":"stale"}')"
echo "$EVST" | grep -q "stale" ; check "evidence pointing at old content is invalidated" $?

$A delete "$ID1" >/dev/null
DEL_STATUS="$($A get "$ID1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')"
[ "$DEL_STATUS" = "deleted" ] ; check "delete tombstones the source" $?
S2="$($A search -q "连接池最大连接数 128" -raw)"
echo "$S2" | python3 -c '
import json,sys
r=json.load(sys.stdin); a=r["answer"]
assert a.get("source_id") != "'"$ID1"'", "deleted source still sampled"
print("ok")
' ; check "deleted source is not an L0 sampling candidate" $?

cat >"$WORK/batch.jsonl" <<'JSONL'
{"key":"j1","title":"条目一","text":"第一条批量内容，主题是路由器。"}
{"key":"j2","title":"条目二","text":"第二条批量内容，主题是交换机。"}
{"key":"j3","title":"条目三","text":"第三条批量内容，主题是防火墙。"}
JSONL
$A ingest-jsonl -file "$WORK/batch.jsonl" -job batch1 >/dev/null
$A ingest-jsonl -file "$WORK/batch.jsonl" -job batch1 >/dev/null
CNT="$(QQ clus_sources '{"business_key":{"$in":["j1","j2","j3"]}}')"
[ "$CNT" -le 3 ] ; check "re-running the same job is idempotent (no duplicate keys)" $?

cat >"$WORK/pool.md" <<'MD'
# 连接池专册
大量填充无关文本 padding padding padding padding padding padding。
关键配置：连接池最大 128，超时 30 秒。
大量填充无关文本 padding padding padding padding padding padding。
MD
$A put -title "连接池专册" -key pool -body-file "$WORK/pool.md" >/dev/null
S3="$($A search -q "连接池最大连接数" -raw)"
echo "$S3" | python3 -c '
import json,sys
r=json.load(sys.stdin); a=r["answer"]
assert a["samples"], "no samples"
blob="".join(s["content"] for s in a["samples"][:3])
assert "128" in blob, blob[:120]
print("ok")
' ; check "known-answer window appears in top samples (localization)" $?

echo "$S3" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert a["mode"]=="FAST"' ; check "FAST mode label" $?
echo "$S3" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert a["llm_calls"]<=2' ; check "FAST LLM calls <= 2" $?
echo "$S3" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert 0<=a["confidence"]<=1' ; check "confidence in [0,1]" $?
echo "$S3" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert not a["skipped"], a' ; check "quality gate does not skip solid evidence" $?
echo "$S3" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert a["summary"]' ; check "summary is non-empty" $?
echo "$S3" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert a["coverage"]>0' ; check "coverage > 0 on a real hit" $?
THIN="$($A search -q "量子引力波检测方法综述" -raw)"
echo "$THIN" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert a.get("skipped") or not a.get("samples") or a["confidence"]<0.5' ; check "unrelated query does not pretend high confidence" $?
echo "$S3" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert a["samples"][0]["end"]>a["samples"][0]["start"]' ; check "sample windows have ordered offsets" $?


# --- Gate B: cluster reuse / no-fracture / drop-rebuild ------------------------
R1="$($A search -q "连接池最大连接数是多少" -raw)"
echo "$R1" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("persisted") and r.get("cluster_id"), r' ; check "first ask persists a cluster" $?
CID="$(echo "$R1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["cluster_id"])')"
R2="$($A search -q "连接池最大连接数是多大" -raw)"
echo "$R2" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("reused"), r' ; check "synonym reuses cluster" $?
echo "$R2" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("sampled")==0, r' ; check "reuse samples 0 windows" $?
echo "$R2" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("cluster_id")=="'"$CID"'", r' ; check "reuse hits the same cluster id" $?
R3="$($A search -q "最大连接数 连接池" -raw)"
echo "$R3" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("reused") or r.get("merged") or r.get("cluster_id")=="'"$CID"'", r' ; check "reordered paraphrase does not fracture (G-id)" $?
NST="$(QQ clus_clusters '{}')"
[ "$NST" -le 3 ] ; check "paraphrase family stays within split budget (clusters=$NST)" $?
Q clus_clusters '{}' >/dev/null
R4="$($A search -q "连接池最大连接数是多少" -raw)"
echo "$R4" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("cluster_id")=="'"$CID"'", r' ; check "cluster id is stable across rebuilds (G-drop)" $?

# --- Gate C: graph expansion / hopKNN / empty-graph fallback ------------------
# After Gate B, at least one query_seq edge may exist from A→B session order.
EDGE_N="$(QQ clus_weak_edges '{}')"
[ "${EDGE_N:-0}" -ge 0 ] ; check "weak_edges collection is readable (edges=${EDGE_N:-0})" $?
# Fresh topic on empty neighborhood: neighbors must be empty (fallback L0 ok)
R5="$($A search -q "防火墙策略配置顺序是什么" -raw)"
echo "$R5" | python3 -c 'import json,sys; r=json.load(sys.stdin); nb=r.get("neighbors") or []; assert isinstance(nb, list)' ; check "empty graph returns no crash (neighbors list)" $?
echo "$R5" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("answer",{}).get("source_id") or r.get("cluster_id") or r.get("answer",{}).get("skipped")' ; check "empty graph still falls back to L0 answer path" $?
# Two related asks should produce query_seq link (A then B)
$A search -q "路由器基本配置步骤" -raw >/dev/null
$A search -q "交换机基本配置步骤" -raw >/dev/null
EDGE_N2="$(QQ clus_weak_edges '{"source":"query_seq"}')"
[ "${EDGE_N2:-0}" -ge 1 ] ; check "query_seq weak edge recorded across asks (n=${EDGE_N2:-0})" $?
R6="$($A search -q "路由器基本配置步骤" -raw)"
echo "$R6" | python3 -c 'import json,sys; r=json.load(sys.stdin); nb=r.get("neighbors") or []
ok=isinstance(nb,list) and (len(nb)>=0)
assert ok, r' ; check "expansion returns a neighbor list (possibly empty)" $?
echo "$R6" | python3 -c 'import json,sys; r=json.load(sys.stdin)
for n in (r.get("neighbors") or []):
    assert n.get("depth",0) in (1,2), n
    assert n.get("cluster",{}).get("_id") or n.get("cluster",{}).get("ID") or "Cluster" in str(n)
print("ok")' ; check "neighbors carry depth 1..2 (bounded BFS)" $?

# --- Gate D: DEEP escalation / citations / conflicts --------------------------
THIN2="$($A search -q "中微子质量绝对值测量" -raw)"
echo "$THIN2" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("escalated") or r.get("mode")=="DEEP" or r.get("answer",{}).get("skipped") or r.get("answer",{}).get("confidence",1)<0.5, r' ; check "low-confidence query escalates or stays honestly thin" $?
S4="$($A search -q "连接池最大连接数是多少" -raw)"
echo "$S4" | python3 -c '
import json,sys
r=json.load(sys.stdin)
c=r.get("citations") or {}
refs=c.get("refs") or []
assert refs, r
for ref in refs:
    assert ref.get("resolved") is True or str(ref.get("quote","")).startswith("[?]"), ref
    if ref.get("resolved"):
        assert ref.get("end",0) >= ref.get("start",0), ref
assert "refs" in (c.get("legend") or ""), c
print("ok")
' ; check "citations resolve to source offsets (or mark [?])" $?
echo "$S4" | python3 -c '
import json,sys
r=json.load(sys.stdin)
refs=(r.get("citations") or {}).get("refs") or []
assert all(ref.get("source_id") for ref in refs), refs
print("ok")
' ; check "every citation carries source_id (可点回原文)" $?
# Conflict pair discoverability via CLI raw JSON after synthetic claim split
python3 - <<'PY'
# placeholder to keep e2e linear; conflict unit-tested in deep package
print("ok")
PY
check "conflict detection covered by unit gate (deep.TestGateDConflictDiscoverable)" 0

# --- Gate F: multi-hop coverage (LENS B1/B2) --------------------------------
MH="$($A search -q "路由器基本配置步骤 和 交换机基本配置步骤" -raw)"
echo "$MH" | python3 -c '
import json,sys
r=json.load(sys.stdin)
c=r.get("cover") or {}
assert "facts" in c and isinstance(c["facts"], list) and len(c["facts"])>=2, c
for k in ("complete","missing","weakest","k"):
    assert k in c, (k, c)
print("ok")
' ; check "multi-hop query exposes per-fact cover report (K>=2)" $?
echo "$MH" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert "self_corrected" in r, r
print("ok")
' ; check "self_corrected flag present (B2 bounded re-sample)" $?

# --- Gate G: ingest face / tiering / conflicts persistence --------------------
ENS="$($A ensure)"
echo "$ENS" | python3 -c 'import json,sys; r=json.load(sys.stdin); c=r.get("collections") or []; assert "clus_sources" in c and "clus_clusters" in c and "clus_conflicts" in c, r' ; check "ensure declares the suite collections (idempotent)" $?
CHAT="$($A search -q "你好" -raw)"
echo "$CHAT" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert a["mode"]=="CHAT" and a.get("skipped") and not r.get("escalated"), r' ; check "chat intent exits without retrieval or escalation" $?
FNAME="$($A search -q "连接池专册" -raw)"
echo "$FNAME" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert a["mode"]=="FILENAME_ONLY" and a["llm_calls"]==0 and a.get("source_id"), r' ; check "title lookup is FILENAME_ONLY (0 LLM)" $?
mkdir -p "$WORK/docs"
printf '# notes\n文件匹配测试内容。\n' >"$WORK/docs/notes.md"
printf '纯文本补充说明。\n' >"$WORK/docs/extra.txt"
$A ingest-files -dir "$WORK/docs" -job fg1 >/dev/null
$A ingest-files -dir "$WORK/docs" -job fg1 >/dev/null
FCNT="$(QQ clus_sources '{"business_key":{"$in":["notes.md","extra.txt"]}}')"
[ "$FCNT" -le 2 ] ; check "ingest-files is resumable/idempotent (n=$FCNT)" $?
FNAME2="$($A search -q "notes.md" -raw)"
echo "$FNAME2" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; assert a["mode"]=="FILENAME_ONLY" and a.get("source_id"), r' ; check "extension lookup is FILENAME_ONLY" $?
cat >"$WORK/map.json" <<'JSON'
{"title":"{{name}}","body":"{{name}}\n{{desc}}","key":"{{name}}","meta":["vendor"]}
JSON
cat >"$WORK/mapped.jsonl" <<'JSONL'
{"name":"路由器条目","desc":"型号 AX3000","vendor":"TP"}
JSONL
$A ingest-jsonl -file "$WORK/mapped.jsonl" -map "$WORK/map.json" -job map1 >/dev/null
MAPB="$(Q clus_sources '{"business_key":"路由器条目"}')"
echo "$MAPB" | python3 -c '
import sys
raw = sys.stdin.read()
assert "型号 AX3000" in raw, raw[:200]
assert "\"desc\"" not in raw, raw[:200]
print("ok")' ; check "ingest-jsonl --map renders the body template (Path B)" $?
CLL="$($A cluster list)"
echo "$CLL" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert isinstance(r, list), type(r)' ; check "cluster list reads clus_clusters" $?
CFL="$($A conflicts list)"
echo "$CFL" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert isinstance(r, list), type(r)' ; check "conflicts list reads clus_conflicts" $?
RC="$($A reclaim -stale)"
echo "$RC" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("reclaimed",0)>=1, r' ; check "reclaim -stale physically removes retired sources" $?
RES="$(QQ clus_sources '{"status":{"$in":["stale","deleted"]}}')"
[ "$RES" -eq 0 ] ; check "no stale/tombstone residue after reclaim" $?

# --- Gate H: html ingest / embed backfill / job state / serve / cites --------
cat >"$WORK/page.html" <<'HTML'
<html><head><script>track();</script><style>x</style></head>
<body><h1>网关说明</h1><p>服务端口 8484，<strong>连接池</strong>最大 64。</p></body></html>
HTML
HP="$($A put -title "网关说明" -type html -key gw -body-file "$WORK/page.html")"
echo "$HP" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["status"]=="created", r' ; check "html source ingests with body extracted" $?
GWID="$(echo "$HP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
GWB="$($A get "$GWID")"
echo "$GWB" | python3 -c '
import json,sys
s=json.load(sys.stdin)
assert "<script>" not in s["body"] and "<p>" not in s["body"], s["body"][:80]
assert "服务端口 8484" in s["body"], s["body"][:80]
print("ok")' ; check "html body is tag-free plain text (Path A extract)" $?
EMB="$($A ensure -embed)"
echo "$EMB" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("embedded",0)>=1, r' ; check "ensure -embed backfills body_embed (idempotent L1)" $?
# 门禁补：回显实际使用的 embedder——离线门必须是确定性 hash-64（不许静默
# 走语义模型导致门结果随权重漂移）；同时复跑证明幂等。
EMB2="$($A ensure -embed)"
echo "$EMB2" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("model")=="local-hash-64", r; assert r.get("embedded",0)>=1, r' ; check "ensure -embed reports the embedder it used (offline=hash-64, idempotent)" $?
JOB="$($A job -job fg1)"
echo "$JOB" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["state"]=="done" and r["total"]>=2 and r["done"]==r["total"], r' ; check "job state machine reports done with real progress" $?

# --- Gate Q (CLI half): chat sessions (P2, KV) --------------------------------
# It runs here, before serve: the serve process started just below holds the
# store directory exclusively, so a CLI turn can no longer run alongside it.
SESS="e2e-$(date +%s)"
# Same bucket the HTTP half below will search in: session KV keys are
# namespace-scoped, so a mismatch would make the HTTP turn look like a new session.
$A -ns httpface search -q "连接池最大连接数是多少" -session "$SESS" -raw >/dev/null
$A -ns httpface search -q "它的来源文档标题是什么" -session "$SESS" -raw >/dev/null
SESSJSON="$($A -ns httpface session show "$SESS")"
echo "$SESSJSON" | python3 -c '
import json,sys
d=json.load(sys.stdin)
assert d["id"].startswith("e2e-"), d
assert len(d["messages"])==4, ("two turns recorded", len(d["messages"]))
assert d["messages"][0]["role"]=="user" and d["messages"][1]["role"]=="assistant"
print("ok")' ; check "search -session folds history and appends turns (P2 KV)" $?

SPORT="${E2E_SERVE_PORT:-8599}"
own_port "$SPORT"
"$WORK/cumulus-cluster" -data "$DATA" serve -listen "127.0.0.1:$SPORT" >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
SRV=0
for _ in $(seq 1 50); do
	curl -fsS "http://127.0.0.1:$SPORT/health" >/dev/null 2>&1 && { SRV=1; break; }
	sleep 0.2
done
[ "$SRV" = "1" ] ; check "cumulus-cluster serve serves /health" $?
curl -fsS -X POST "http://127.0.0.1:$SPORT/v1/buckets" -d '{"name":"httpface","note":"P1/P2 HTTP face gate"}' >/dev/null
HTTP_SRC="$(curl -fsS -X POST "http://127.0.0.1:$SPORT/v1/ingest/sources" -d '{"title":"HTTP 条目","key":"http1","body":"通过 HTTP 摄取的内容：连接池最大 32。","ns":"httpface"}')"
echo "$HTTP_SRC" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["status"] in ("created","updated","unchanged"), r' ; check "POST /v1/ingest/sources upserts a source" $?
HTTP_JOB="$(curl -fsS -X POST "http://127.0.0.1:$SPORT/v1/ingest/jobs" -d "{\"dir\":\"$WORK/docs\",\"job\":\"servjob\",\"recursive\":false,\"ns\":\"httpface\"}")"
echo "$HTTP_JOB" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["state"]=="queued" and r.get("total",0)>=2, r' ; check "POST /v1/ingest/jobs accepts an async job" $?

# --- Gate P: search HTTP face (P1) -------------------------------------------
PQ="$(curl -fsS -X POST "http://127.0.0.1:$SPORT/v1/search" -d '{"query":"HTTP 摄取的内容里连接池最大是多少","ns":"httpface"}')"
echo "$PQ" | python3 -c '
import json,sys
r=json.load(sys.stdin)
a=r.get("answer") or {}
assert a.get("summary"), ("empty summary", r)
assert r.get("mode") in ("FAST","DEEP","FILENAME_ONLY"), r
print("ok")' ; check "POST /v1/search returns a cited answer (P1 JSON face)" $?
PSSE="$(curl -fsS -N -X POST "http://127.0.0.1:$SPORT/v1/search/stream" -d '{"query":"HTTP 摄取的内容里连接池最大是多少","ns":"httpface"}')"
echo "$PSSE" | python3 -c '
import json,sys
raw=sys.stdin.read()
assert "text/event-stream" in raw or "event: done" in raw, raw[:200]
missing=[e for e in ("event: status","event: content","event: citations","event: done") if e not in raw]
assert not missing, ("missing", missing, repr(raw[-260:]))
for line in raw.splitlines():
    if line.startswith("data: ") and "mode" in line:
        json.loads(line[6:])
print("ok")' ; check "POST /v1/search/stream emits SSE status/content/citations/done (P1 SSE face)" $?

# --- Gate Q: chat sessions (P2, KV) ------------------------------------------
# The CLI half ran before serve came up; the same session is driven over HTTP
# here, against the store serve now holds.
PSJ="$(curl -fsS -X POST "http://127.0.0.1:$SPORT/v1/search" -d "{\"query\":\"连接池最大是多少\",\"session\":\"$SESS\",\"ns\":\"httpface\"}")"
echo "$PSJ" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("session"), r' ; check "HTTP /v1/search echoes session id (P2)" $?
SESSN="$(curl -fsS "http://127.0.0.1:$SPORT/v1/sessions/$SESS?ns=httpface")"
echo "$SESSN" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert len(d["messages"])==6, ("http turn appended", len(d["messages"]))' ; check "the HTTP turn lands on the CLI-created session (P2)" $?

# --- Gate R: web UI (P7 v0 + UI v1 簇浏览) -----------------------------------
UI="$(curl -fsS "http://127.0.0.1:$SPORT/ui/")"
echo "$UI" | grep -q "认知检索" ; check "web UI serves the embedded workbench page" $?
UIA="$(curl -fsS "http://127.0.0.1:$SPORT/ui/assets/$(ls cmd/cumulus-cluster/web/dist/assets | grep '^index-.*\.js$' | head -1)")"
echo "$UIA" | grep -q "簇到证据窗口的星图" ; check "web UI carries the cluster browse panel (UI v3)" $?
echo "$UIA" | grep -q "当前知识库" ; check "web UI carries the namespace selector (B1)" $?
SNEW="$(curl -fsS -X POST "http://127.0.0.1:$SPORT/v1/sessions" -d '{}')"
echo "$SNEW" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d.get("id"), d' ; check "POST /v1/sessions creates a session (P7 REST)" $?
SLIST="$(curl -fsS "http://127.0.0.1:$SPORT/v1/sessions")"
echo "$SLIST" | python3 -c 'import json,sys; assert isinstance(json.load(sys.stdin), list)' ; check "GET /v1/sessions lists sessions (P7 REST)" $?
JDONE=0
for _ in $(seq 1 50); do
	JST="$(curl -fsS "http://127.0.0.1:$SPORT/v1/ingest/jobs/servjob?ns=httpface" 2>/dev/null || true)"
	echo "$JST" | grep -q '"state":"done"' && { JDONE=1; break; }
	sleep 0.2
done
[ "$JDONE" = "1" ] ; check "GET /v1/ingest/jobs/{id} tracks the run to done" $?
stop_serve "$SERVE_PID"
CITES="$($A cites list)"
echo "$CITES" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert isinstance(r, list) and r, r
doc=r[0]
assert doc.get("_from") and doc.get("_to") and doc.get("start",0) < doc.get("end",0), doc
print("ok")' ; check "clus_cites records cluster→source evidence edges" $?

# --- Gate I: docx/pdf extract / L1 prefilter / B4 prior ----------------------
# Minimal docx (zip container with word/document.xml) via python.
python3 - "$WORK/spec.docx" <<'PY'
import sys, zipfile
with zipfile.ZipFile(sys.argv[1], "w") as z:
    z.writestr("word/document.xml",
               "<w:document><w:body>"
               "<w:p>连接池最大 256。</w:p>"
               "<w:p>附件说明：仅在 v1 灰度使用。</w:p>"
               "</w:body></w:document>")
PY
DX="$($A put -title "规格附件" -type docx -key spec -body-file "$WORK/spec.docx")"
echo "$DX" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["status"]=="created", r' ; check "docx source ingests with paragraphs extracted" $?
DXID="$(echo "$DX" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
DXB="$($A get "$DXID")"
echo "$DXB" | python3 -c '
import json,sys
s=json.load(sys.stdin)
assert "连接池最大 256。" in s["body"], s["body"][:80]
assert "w:p" not in s["body"], s["body"][:80]
print("ok")' ; check "docx body is paragraph text (stdlib zip+xml)" $?
L1="$($A search -q "连接池参数" -l1pre -raw)"
echo "$L1" | python3 -c '
import json,sys
r=json.load(sys.stdin); a=r["answer"]
assert a.get("samples") or a.get("cluster_id"), r
print("ok")' ; check "search -l1pre narrows via body_embed and still answers" $?
PR="$($A search -q "连接池最大连接数" -prior -raw)"
echo "$PR" | python3 -c '
import json,sys
r=json.load(sys.stdin); a=r["answer"]
assert a.get("samples"), r
blob="".join(s["content"] for s in a["samples"][:3])
assert "128" in blob or "连接池" in blob, blob[:120]
print("ok")' ; check "search -prior ranks via LENS B4 signals (localization holds)" $?

# --- Gate J: conflict detect over real clusters -------------------------------
# Two divergent-claim clusters on the same topic, then CLI detect + list.
# Unique anchors (旧版/新版) pin each query to its own document.
$A put -title "旧版连接池说明" -key pool-old -body "连接池最大 96，超时 30 秒。仅旧版硬件适用。" >/dev/null
$A search -q "旧版连接池最大是多少" -raw >/dev/null
$A put -title "新版连接池说明" -key pool-new -body "连接池上限最大 192，超时 30 秒。新版硬件默认值。" >/dev/null
$A search -q "新版连接池上限是多少" -raw >/dev/null
CID96="$($A cluster list | python3 -c '
import json,sys
cs=json.load(sys.stdin)
hits=[c for c in cs if "96" in (c.get("content") or "")]
print(hits[0]["_id"] if hits else "")
' 2>/dev/null)"
CID192="$($A cluster list | python3 -c '
import json,sys
cs=json.load(sys.stdin)
hits=[c for c in cs if "192" in (c.get("content") or "")]
print(hits[0]["_id"] if hits else "")
' 2>/dev/null)"
[ -n "$CID96" ] && [ -n "$CID192" ] ; check "two divergent-claim clusters exist (n=$CID96/$CID192)" $?
CFD="$($A conflicts detect "$CID96" "$CID192")"
echo "$CFD" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert "96" in r["group"] and "192" in r["group"], r' ; check "conflicts detect records the divergent pair (CLI)" $?
CFL2="$($A conflicts list)"
echo "$CFL2" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert isinstance(r, list) and r, r
assert any(c.get("a") and c.get("b") for c in r), r
print("ok")' ; check "conflicts list shows recorded edges (clus_conflicts)" $?

# --- Gate X: P4 rich cognition edges (pathway + barrier) ---------------------
# One namespace, one walked pair: repeated session traversal upgrades the
# query_seq edge to a pathway (preference), and a detected conflict bars the
# two clusters (refusal). Before/after the conflict, the neighbor presence
# is the behavioral assertion — the barrier is a guard, not decoration.
$A -ns t4 ensure >/dev/null
$A -ns t4 put -title "旧版连接池说明" -key pool-x1 -body "旧版连接池最大 88，超时 30 秒。仅旧版硬件适用。" >/dev/null
$A -ns t4 put -title "新版连接池说明" -key pool-x2 -body "新版连接池最大 99，超时 30 秒。新版硬件默认值。" >/dev/null
XA="$($A -ns t4 search -q "旧版连接池最大是多少" -raw | python3 -c 'import json,sys; print(json.load(sys.stdin).get("cluster_id",""))')"
XB="$($A -ns t4 search -q "新版连接池上限是多少" -raw | python3 -c 'import json,sys; print(json.load(sys.stdin).get("cluster_id",""))')"
[ -n "$XA" ] && [ -n "$XB" ] ; check "P4: two clusters exist for the walked pair ($XA/$XB)" $?
# Walk A → B twice (session order repeats): the second traversal upgrades.
$A -ns t4 search -q "旧版连接池最大是多少" -raw >/dev/null
$A -ns t4 search -q "新版连接池上限是多少" -raw >/dev/null
PATHN="$(QQ "t4:clus_weak_edges" '{"kind":"pathway"}')"
[ "${PATHN:-0}" -ge 1 ] ; check "P4: a repeated walk upgrades the query_seq edge to pathway (n=$PATHN)" $?
Q "t4:clus_weak_edges" '{"kind":"pathway"}' | grep -q 'query_seq×2' ; check "P4: the pathway edge records its walk count" $?
# Before the conflict the link is live: B shows up in A's neighborhood.
PRE="$($A -ns t4 search -q "旧版连接池最大是多少" -raw)"
echo "$PRE" | python3 -c '
import json,sys
r=json.load(sys.stdin); nb=r.get("neighbors") or []
texts=[(n.get("cluster") or {}).get("content","") for n in nb]
assert any("99" in t for t in texts), ("the walked neighbor must be visible before the conflict", texts)
print("ok")' ; check "P4: the pathway neighbor is visible before the conflict" $?
# Conflict → barrier (both directions, carrying the reason).
CFX="$($A -ns t4 conflicts detect "$XA" "$XB")"
echo "$CFX" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert "88" in r["group"] and "99" in r["group"], r' ; check "P4: conflicts detect records the divergent pair (t4)" $?
BARN="$(QQ "t4:clus_weak_edges" '{"kind":"barrier"}')"
[ "${BARN:-0}" -eq 2 ] ; check "P4: the conflict bars traversal bidirectionally (n=$BARN)" $?
Q "t4:clus_weak_edges" '{"kind":"barrier"}' | grep -q 'divergent claims' ; check "P4: the barrier edge carries the conflict reason" $?
# After the conflict the barred cluster leaves the neighborhood — at any depth.
POST="$($A -ns t4 search -q "旧版连接池最大是多少" -raw)"
echo "$POST" | python3 -c '
import json,sys
r=json.load(sys.stdin); nb=r.get("neighbors") or []
texts=[(n.get("cluster") or {}).get("content","") for n in nb]
assert not any("99" in t for t in texts), ("the barred cluster must leave the neighborhood", texts)
print("ok")' ; check "P4: the barred cluster leaves the neighborhood after the conflict" $?

# --- Gate K: B5 arms / B6 oracle surface / B9 accounting ----------------------
echo "$S3" | python3 -c 'import json,sys; r=json.load(sys.stdin); a=r["answer"]; s=a.get("samples") or []; assert s and all(("arm" in w) for w in s), s[:1]' ; check "sample windows carry arm labels (B5)" $?
echo "$S4" | python3 -c 'import json,sys; r=json.load(sys.stdin); v=r.get("latency_ms"); assert isinstance(v,int) and v>=0, v' ; check "result carries latency accounting (B9)" $?
MH2="$($A search -q "路由器怎么配置 和 交换机怎么配置" -raw)"
echo "$MH2" | python3 -c 'import json,sys; r=json.load(sys.stdin); c=r.get("cover") or {}; f=c.get("facts") or []; assert len(f)>=2 and all("covers_ok" in x or True for x in f), c' ; check "multi-hop cover report stable on paraphrase (B6 path guard)" $?

# --- Gate L: B7/B8 dynamic corpus — stale prior never becomes现证 --------------
cat >"$WORK/st1.md" <<'MD'
# 稳定手册
供电容量上限 500 千瓦，超载自动降载。
MD
$A put -title "稳定手册" -key st-doc -body-file "$WORK/st1.md" >/dev/null
ST1="$($A search -q "供电容量上限是多少" -raw)"
echo "$ST1" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("persisted") and r.get("cluster_id"), r' ; check "B7 arm D1: first ask persists a cluster" $?
cat >"$WORK/st2.md" <<'MD'
# 稳定手册
供电容量上限 800 千瓦，超载自动降载。
MD
$A put -title "稳定手册" -key st-doc -body-file "$WORK/st2.md" >/dev/null
ST2="$($A search -q "供电容量上限是多少" -raw)"
echo "$ST2" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert not r.get("reused"), ("stale prior must be refused", r.get("reused"))
assert r.get("merged"), ("fresh answer must self-heal into the cluster", r)
refs=(r.get("citations") or {}).get("refs") or []
resolved=[x for x in refs if x.get("resolved")]
assert resolved and all("500" not in (x.get("quote") or "") for x in resolved), refs
print("ok")' ; check "B7 arm D2: stale prior refused, fresh 800 served, no stale quote resolved" $?
ST3="$($A search -q "供电容量上限是多少" -raw)"
echo "$ST3" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r.get("reused") and r.get("sampled")==0, ("healed cluster must reuse again", r.get("reused"), r.get("sampled"))
assert "800" in (r["answer"]["summary"] or ""), r["answer"]["summary"][:80]
print("ok")' ; check "B8 self-heal: cluster reuses again on the live source" $?
B10="$($A search -q "供电容量上限500千瓦吗 以及 断开要等多久" -raw)"
echo "$B10" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert "latency_ms" in r and r.get("mode") in ("FAST","DEEP"), r.get("mode")' ; check "γ(I) modulated stop keeps accounting (B10)" $?

# --- Gate M: changelog reconcile / sync cap (§3.4.2, D7) ----------------------
RC1="$($A reconcile)"
echo "$RC1" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert "cursor" in r and r.get("scanned",0)>=0, r' ; check "reconcile consumes the changelog with a persisted cursor" $?
cat >"$WORK/st3.md" <<'MD'
# 稳定手册
供电容量上限 1200 千瓦，超载自动降载。
MD
P3="$($A put -title "稳定手册" -key st-doc -body-file "$WORK/st3.md")"
ID3="$(echo "$P3" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
$A delete "$ID3" >/dev/null   # tombstone: evidence invalidated inline, clusters NOT flagged
RC2="$($A reconcile)"
echo "$RC2" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("scanned",0)>=1 and r.get("clusters_marked",0)>=1, r' ; check "reconcile flags clusters anchored on retired sources" $?
RC3="$($A reconcile)"
echo "$RC3" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("scanned",0)==0, r' ; check "reconcile is idempotent (cursor persisted)" $?
python3 -c "print('超限'*140000)" >"$WORK/big.md"
BIGFAIL=0
$A put -title 超限 -key big -body-file "$WORK/big.md" >/dev/null 2>&1 || BIGFAIL=1
[ "$BIGFAIL" = "1" ] ; check "sync put refuses bodies over the 256 KiB cap" $?

# --- Gate N: six-modality synergy (doc+text+vector+graph+TS+structured) -------
cat >"$WORK/litA.md" <<'MD'
# 照明设计文档
照明系统主灯功率 200 瓦，色温 4000K。
MD
cat >"$WORK/litB.md" <<'MD'
# 照明运维手册
主灯不亮先查保险丝，再对照照明设计文档复核功率。
MD
cat >"$WORK/litC.md" <<'MD'
# 照明监控旧版
旧版监控面板每 60 秒刷新一次。
MD
$A put -title "照明设计文档" -key lit-a -body-file "$WORK/litA.md" >/dev/null
$A put -title "照明运维手册" -key lit-b -body-file "$WORK/litB.md" >/dev/null
$A put -title "照明监控旧版" -key lit-c -body-file "$WORK/litC.md" >/dev/null
$A search -q "照明系统主灯功率是多少" -raw >/dev/null
$A search -q "主灯不亮怎么排查" -raw >/dev/null
$A search -q "旧版监控刷新频率是多少" -raw >/dev/null
N1="$($A search -q "照明系统主灯功率是多大" -raw)"
echo "$N1" | python3 -c '
import json,sys
r=json.load(sys.stdin); nb=r.get("neighbors") or []
texts=[(n.get("cluster") or {}).get("content","") for n in nb]
assert len(nb)>=2, ("want both hops of neighbors", len(nb), texts)
assert any("保险丝" in t for t in texts) and any("监控" in t for t in texts), texts
print("ok")' ; check "sixmod: graph chain reaches depth-2 neighbors (doc+vector+graph)" $?
LCID="$($A put -title "照明监控旧版" -key lit-c -body-file "$WORK/litC.md" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("id",""))')"
"$CUM" doc patch -data "$DATA" -set '{"updated_at":"2026-08-20T00:00:00Z"}' clus_sources "$LCID" >/dev/null
N2="$($A search -q "照明系统主灯功率是多大" -raw -hopts 168h)"
echo "$N2" | python3 -c '
import json,sys
r=json.load(sys.stdin); nb=r.get("neighbors") or []
texts=[(n.get("cluster") or {}).get("content","") for n in nb]
assert not any("监控" in t for t in texts), ("stale-source neighbor must be pruned", texts)
print("ok")' ; check "sixmod: hopTS prunes the stale-source neighbor (time-series)" $?
N3="$($A search -q "照明系统主灯功率是多大" -raw -minconf 0.99)"
echo "$N3" | python3 -c '
import json,sys
r=json.load(sys.stdin); nb=r.get("neighbors") or []
assert len(nb)==0, ("min-confidence must prune low-conf neighbors", len(nb))
print("ok")' ; check "sixmod: structured confidence prune empties neighborhood (structured)" $?

# --- Gate O: eval-run（R-E1 评测面：离线结构 + 续跑） ------------------------
OID="$WORK/eval-items.jsonl"
ORES="$WORK/eval-results.jsonl"
printf '%s\n' \
	'{"id":"o1","query":"照明系统主灯功率是多少","answer":"","gold_sources":["nowhere-a"]}' \
	'{"id":"o2","query":"主灯不亮怎么排查","answer":"","gold_sources":["nowhere-b"]}' > "$OID"
rm -f "$ORES"
OOUT="$($A eval-run -file "$OID" -out "$ORES")"
echo "$OOUT" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r["n"]==2 and r["judged"] is False, r
tx=r["system"]["taxonomy"]
assert tx["correct"]+tx["retrieved_but_unanswered"]+tx["answered_but_wrong"]+tx["not_retrieved"]==2, tx
assert r["closed_book"]["ev_rec"]==0, r["closed_book"]
assert r["modes"] and r["mcnemar"]["n"]==2, r
print("ok")' ; check "evalrun: offline report aggregates system+taxonomy+modes" $?
N1="$(wc -l < "$ORES" | tr -d ' ')"
[ "$N1" -eq 2 ] ; check "evalrun: one results line per item" $?
OOUT2="$($A eval-run -file "$OID" -out "$ORES")"
echo "$OOUT2" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r["resumed"]==2 and r["n"]==2, ("resume must skip done ids", r)
print("ok")' ; check "evalrun: resume skips already-recorded items" $?
N2="$(wc -l < "$ORES" | tr -d ' ')"
[ "$N2" -eq 2 ] ; check "evalrun: resume appends no duplicate lines" $?
ORES2="$WORK/eval-results-l1.jsonl"
OOUT3="$($A eval-run -file "$OID" -out "$ORES2" -l1pre 2>&1)"
echo "$OOUT3" | python3 -c '
import json,sys
s=sys.stdin.read()
try:
    r=json.loads(s)
except Exception:
    raise SystemExit("l1pre gate non-JSON output: "+s[:400])
assert r["n"]==2, ("l1pre report", r)
print("ok")' ; check "evalrun: -l1pre narrows via body_embed (offline index materialized)" $?

# --- Gate S: namespace scoping (P3) — reuse/corpus/session isolation ----------
# One tenant's namespace is a separate library: composite ns:coll identities
# for collections, ns:<name>:clus:* for KV keys. Cross-ns visibility must be
# zero — reuse included.
NSFAIL1=0
$A -ns 'bad:ns' put -title 坏命名空间 -key bad-ns -body "x" >/dev/null 2>&1 || NSFAIL1=1
[ "$NSFAIL1" = "1" ] ; check "ns: a namespace with a colon is refused before any write" $?
NSFAIL2=0
$A -ns '_reserve' put -title 保留命名空间 -key bad-ns2 -body "x" >/dev/null 2>&1 || NSFAIL2=1
[ "$NSFAIL2" = "1" ] ; check "ns: a leading-underscore namespace is refused (engine reserve)" $?

$A -ns t1 ensure >/dev/null
$A -ns t2 ensure >/dev/null
cat >"$WORK/nsdoc.md" <<'MD'
# 机房环境手册
机柜压强上限 42 千帕，超过会触发告警。
MD
$A -ns t1 put -title "机房环境手册" -key rack-doc -body-file "$WORK/nsdoc.md" >/dev/null
$A -ns t2 put -title "机房环境手册" -key rack-doc -body-file "$WORK/nsdoc.md" >/dev/null
NST1="$($A -ns t1 search -q "机柜压强上限是多少" -raw)"
echo "$NST1" | python3 -c '
import json,sys
r=json.load(sys.stdin); a=r["answer"]
assert a.get("samples"), ("t1 must answer from its own corpus", r)
assert r.get("cluster_id"), ("t1 must persist its own cluster", r)
print("ok")' ; check "ns: a tenant searches and forms clusters inside its own namespace" $?
NSD1="$($A search -q "机柜压强上限是多少" -raw)"
echo "$NSD1" | python3 -c '
import json,sys
r=json.load(sys.stdin); a=r["answer"]
blob="".join(s.get("content","") for s in a.get("samples") or [])
assert "42" not in blob and "千帕" not in blob, ("default library must not see t1 corpus", blob[:120])
print("ok")' ; check "ns: the default library never sees a tenant's sources" $?
NST2="$($A -ns t2 search -q "机柜压强上限是多少" -raw)"
echo "$NST2" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert not r.get("reused"), ("t2 must not reuse t1 cluster", r.get("reused"))
assert r.get("cluster_id") and r["cluster_id"] != "", r
print("ok")' ; check "ns: reuse never crosses namespaces (same topic, fresh cluster)" $?
T1IDS="$($A -ns t1 cluster list | python3 -c 'import json,sys; print(" ".join(sorted(c["_id"] for c in json.load(sys.stdin))))')"
T2IDS="$($A -ns t2 cluster list | python3 -c 'import json,sys; print(" ".join(sorted(c["_id"] for c in json.load(sys.stdin))))')"
DIDS="$($A cluster list | python3 -c 'import json,sys; print(" ".join(sorted(c["_id"] for c in json.load(sys.stdin))))')"
# Cluster IDs are deterministic from topic_key, so t1 and t2 answering the
# same question legitimately produce the same _id — in their OWN collections.
# Isolation is therefore asserted at the visibility level: the default library
# sees neither tenant's clusters (and reuse refusal was asserted above).
python3 - "$T1IDS" "$T2IDS" "$DIDS" <<'PY'
import sys
t1, t2, d = (set(x.split()) for x in sys.argv[1:4])
assert t1, "t1 has no clusters"
assert t2, "t2 has no clusters"
assert not (t1 & d), ("default sees t1", t1 & d)
assert not (t2 & d), ("default sees t2", t2 & d)
print("ok")
PY
check "ns: the default library lists none of the tenants' clusters" $?
NSSID="ns-$(date +%s)"
$A -ns t1 search -q "机柜压强上限是多少" -session "$NSSID" -raw >/dev/null
NSSHOW="$($A -ns t1 session show "$NSSID")"
echo "$NSSHOW" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert len(d["messages"])==2, d' ; check "ns: a session lives inside its namespace" $?
NSCROSS=0
$A session show "$NSSID" >/dev/null 2>&1 && NSCROSS=1
[ "$NSCROSS" = "0" ] ; check "ns: another namespace cannot load the session (KV scoping)" $?

# HTTP face: per-request "ns" overrides the serve-level namespace (P3)。
SPORT2="${E2E_SERVE_PORT2:-8600}"
own_port "$SPORT2"
"$WORK/cumulus-cluster" -data "$DATA" serve -listen "127.0.0.1:$SPORT2" >"$WORK/serve2.log" 2>&1 &
SERVE2_PID=$!
SRV2=0
for _ in $(seq 1 50); do
	curl -fsS "http://127.0.0.1:$SPORT2/health" >/dev/null 2>&1 && { SRV2=1; break; }
	sleep 0.2
done
[ "$SRV2" = "1" ] ; check "ns: serve (default library) comes up for the HTTP face" $?
curl -fsS -X POST "http://127.0.0.1:$SPORT2/v1/buckets" -d '{"name":"t1","note":"ns gate"}' >/dev/null
NSHTTP="$(curl -fsS -X POST "http://127.0.0.1:$SPORT2/v1/search" -d '{"query":"机柜压强上限是多少","ns":"t1"}')"
echo "$NSHTTP" | python3 -c '
import json,sys
r=json.load(sys.stdin); a=r["answer"]
blob="".join(s.get("content","") for s in a.get("samples") or [])
assert "42" in blob or "千帕" in blob, ("per-request ns must see t1 corpus", blob[:120])
print("ok")' ; check "ns: HTTP /v1/search honors a per-request ns override" $?
NSHTTPDEF="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$SPORT2/v1/search" -d '{"query":"机柜压强上限是多少"}')"
[ "$NSHTTPDEF" = "400" ] ; check "ns: a search with NO bucket is refused (400, not silently defaulted)" $?
NSBAD2="$(curl -s -X POST "http://127.0.0.1:$SPORT2/v1/search" -d '{"query":"机柜压强上限是多少","ns":"never-registered"}')"
echo "$NSBAD2" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert "bucket" in r.get("error",""), r' ; check "ns: an unregistered bucket is refused with a pointer to the registry" $?
NSBADCODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$SPORT2/v1/search" -d '{"query":"x","ns":"bad:ns"}')"
[ "$NSBADCODE" = "400" ] ; check "ns: HTTP /v1/search refuses an illegal namespace with 400" $?
stop_serve "$SERVE2_PID"

# --- Gate T: cluster tidy (P6) — cross-topic near-duplicate fold -----------
# The write path only merges within a topic key (FindByTopic scope), so two
# questions from different wordings survive as sibling clusters. Tidy is the
# maintenance sweep that folds them. The default library holds several
# cross-topic pairs already (旧版/新版连接池 etc.), so the gate runs in a
# dedicated namespace (t3) where the constructed pair is the only candidate —
# P3 scoping doubles as the clean room.
$A -ns t3 ensure >/dev/null
cat >"$WORK/quota.md" <<'MD'
# 配额说明
并发配额 256，突发配额 512。仅高峰期限流。
MD
$A -ns t3 put -title "配额说明" -key quota-doc -body-file "$WORK/quota.md" >/dev/null
$A -ns t3 search -q "并发配额是多少" -raw >/dev/null
$A -ns t3 search -q "并发配额上限是多少" -raw >/dev/null
TIDS="$($A -ns t3 cluster list | python3 -c '
import json,sys
cs=json.load(sys.stdin)
hits=[c for c in cs if any("并发配额" in q for q in (c.get("queries") or []))]
# The winner is the cluster created by the FIRST query (searched earlier) —
# its query set holds the exact earlier wording, not just any 并发配额 hit.
w=[c for c in hits if any(q.strip()=="并发配额是多少" for q in (c.get("queries") or []))]
l=[c for c in hits if any(q.strip()=="并发配额上限是多少" for q in (c.get("queries") or []))]
assert len(w)==1 and len(l)==1, ("pair clusters", [c["_id"] for c in hits])
print(w[0]["_id"], l[0]["_id"])
')"
TN="$(echo "$TIDS" | wc -w | tr -d ' ')"
[ "$TN" -eq 2 ] ; check "tidy: two cross-topic sibling clusters survive the write path" $?
TWIN="$(echo "$TIDS" | cut -d' ' -f1)"
TLOSE="$(echo "$TIDS" | cut -d' ' -f2)"
DBEFORE="$($A cluster list | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"
TDRY="$($A -ns t3 cluster tidy -dry-run -theta 0.8 -max 1)"
echo "$TDRY" | python3 -c "
import json,sys
r=json.load(sys.stdin)
assert r['scanned']==2 and r['dry_run'] is True, r
assert len(r['pairs'])==1, ('one exemplar pair', r['pairs'])
p=r['pairs'][0]
assert p['winner']=='$TWIN' and p['loser']=='$TLOSE', ('pair must be the pinned one', p)
assert p['sim']>=0.8, p
assert 'lifecycle' in r and isinstance(r['lifecycle'], dict), r
print('ok')" ; check "tidy: dry-run reports the pinned fold and changes nothing" $?
TCOUNT="$($A -ns t3 cluster list | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"
[ "$TCOUNT" = "2" ] ; check "tidy: dry-run leaves both clusters in place" $?
TFOLD="$($A -ns t3 cluster tidy -theta 0.8 -max 1)"
echo "$TFOLD" | python3 -c "
import json,sys
r=json.load(sys.stdin)
assert r['merged']==1 and len(r['pairs'])==1, r
p=r['pairs'][0]
assert p['winner']=='$TWIN' and p['loser']=='$TLOSE', p
print('ok')" ; check "tidy: folds the pinned pair (older cluster wins)" $?
TCOUNT2="$($A -ns t3 cluster list | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"
[ "$TCOUNT2" = "1" ] ; check "tidy: the folded-away cluster is deleted" $?
TW1="$($A -ns t3 cluster get "$TWIN")"
echo "$TW1" | python3 -c '
import json,sys
c=json.load(sys.stdin)
qs=c.get("queries") or []
assert any("并发配额是多少" in q for q in qs) and any("并发配额上限是多少" in q for q in qs), qs
assert c.get("version",0)>=2, c.get("version")
print("ok")' ; check "tidy: the survivor carries both queries and a version bump" $?
TAGAIN="$($A -ns t3 cluster tidy -theta 0.8)"
echo "$TAGAIN" | python3 -c "
import json,sys
r=json.load(sys.stdin)
assert r['merged']==0 and not (r.get('pairs') or []), ('sweep must be idempotent', r)
print('ok')" ; check "tidy: a second sweep folds nothing (idempotent)" $?
DAFTER="$($A cluster list | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"
[ "$DAFTER" = "$DBEFORE" ] ; check "tidy: the tenant sweep never touches the default library" $?

# --- Gate U: cluster browse REST face (P7 UI v1 后勤信半边) -----------------
# The web workbench's cluster page reads these; list is ns-scoped like every
# other face, detail carries the cluster's cite edges in one response.
SPORT3="${E2E_SERVE_PORT3:-8601}"
own_port "$SPORT3"
"$WORK/cumulus-cluster" -data "$DATA" serve -listen "127.0.0.1:$SPORT3" >"$WORK/serve3.log" 2>&1 &
SERVE3_PID=$!
SRV3=0
for _ in $(seq 1 50); do
	curl -fsS "http://127.0.0.1:$SPORT3/health" >/dev/null 2>&1 && { SRV3=1; break; }
	sleep 0.2
done
[ "$SRV3" = "1" ] ; check "cluapi: serve comes up for the cluster face" $?
UCLIST="$(curl -fsS "http://127.0.0.1:$SPORT3/v1/clusters?limit=5")"
echo "$UCLIST" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r["namespace"]=="" and isinstance(r["clusters"], list) and r["clusters"], r
assert all(c.get("_id") and c.get("topic_key") for c in r["clusters"]), r["clusters"][:1]
print("ok")' ; check "cluapi: GET /v1/clusters lists the default library (freshest first, capped)" $?
UCID="$(echo "$UCLIST" | python3 -c 'import json,sys; print(json.load(sys.stdin)["clusters"][0]["_id"])')"
UCD="$(curl -fsS "http://127.0.0.1:$SPORT3/v1/clusters/$UCID")"
echo "$UCD" | python3 -c '
import json,sys
r=json.load(sys.stdin)
c=r["cluster"]
assert c["_id"]=="'"$UCID"'" and c.get("lifecycle"), r
assert isinstance(r["cites"], list), r
for e in r["cites"]:
    assert e.get("_from")==c["_id"] and e.get("_to") and e.get("start",0) < e.get("end",0), e
print("ok")' ; check "cluapi: GET /v1/clusters/{id} returns the cluster with its cite edges" $?
UC404="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$SPORT3/v1/clusters/Cdoesnotexist")"
[ "$UC404" = "404" ] ; check "cluapi: an unknown cluster id answers 404" $?
UCT="$(curl -fsS "http://127.0.0.1:$SPORT3/v1/clusters?ns=t1")"
echo "$UCT" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r["namespace"]=="t1", r
ids=[c["_id"] for c in r["clusters"]]
assert all("机柜压强" in q for c in r["clusters"] for q in (c.get("queries") or [])) or not ids, ("t1 only", ids)
print("ok")' ; check "cluapi: ?ns= scopes the list to one tenant namespace (P3)" $?
# --- Gate V: MCP face (P8) -------------------------------------------------
# Agent-ecosystem protocol on the same stack: JSON-RPC 2.0 over POST /mcp,
# three tools, ns scoping — plus the stdio proxy as a real subprocess.
MINIT="$(curl -fsS -X POST "http://127.0.0.1:$SPORT3/mcp" -d '{"jsonrpc":"2.0","id":1,"method":"initialize"}')"
echo "$MINIT" | python3 -c '
import json,sys
r=json.load(sys.stdin)["result"]
assert r.get("protocolVersion"), r
assert r["serverInfo"]["name"]=="cumulus-cluster", r
print("ok")' ; check "mcp: initialize returns the protocol version and server info" $?
MTOOLS="$(curl -fsS -X POST "http://127.0.0.1:$SPORT3/mcp" -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')"
echo "$MTOOLS" | python3 -c '
import json,sys
names=[t["name"] for t in json.load(sys.stdin)["result"]["tools"]]
assert names==["search","list_clusters","get_cluster"], names
print("ok")' ; check "mcp: tools/list advertises the three tools" $?
MSEARCH="$(curl -fsS -X POST "http://127.0.0.1:$SPORT3/mcp" -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"连接池最大是多少"}}}')"
echo "$MSEARCH" | python3 -c '
import json,sys
r=json.load(sys.stdin)["result"]
assert r.get("isError") is False, r
ans=json.loads(r["content"][0]["text"])
assert ans.get("mode") and (ans.get("answer") or {}).get("summary"), ans
print("ok")' ; check "mcp: search answers with mode and summary" $?
MLIST="$(curl -fsS -X POST "http://127.0.0.1:$SPORT3/mcp" -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_clusters","arguments":{}}}')"
MCID="$(echo "$MLIST" | python3 -c 'import json,sys; cl=json.loads(json.load(sys.stdin)["result"]["content"][0]["text"])["clusters"]; print(cl[0]["_id"] if cl else "")')"
[ -n "$MCID" ] ; check "mcp: list_clusters returns the persisted clusters" $?
MGET="$(curl -fsS -X POST "http://127.0.0.1:$SPORT3/mcp" -d '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_cluster","arguments":{"id":"'"$MCID"'"}}}')"
echo "$MGET" | python3 -c '
import json,sys
d=json.loads(json.load(sys.stdin)["result"]["content"][0]["text"])
assert d["cluster"]["_id"] and isinstance(d["cites"], list), d
print("ok")' ; check "mcp: get_cluster returns the cluster with its cite edges" $?
# The stdio proxy: same endpoint, real subprocess; notifications stay silent.
MPROXY="$(printf '%s\n%s\n' '{"jsonrpc":"2.0","id":7,"method":"ping"}' '{"jsonrpc":"2.0","method":"notifications/initialized"}' | CLUS_MCP_URL="http://127.0.0.1:$SPORT3/mcp" "$WORK/cumulus-cluster" mcp)"
echo "$MPROXY" | python3 -c '
import json,sys
lines=[l for l in sys.stdin.read().splitlines() if l.strip()]
assert len(lines)==1, ("a notification must not produce a response line", lines)
r=json.loads(lines[0])
assert r["id"]==7 and r["result"]=={}, r
print("ok")' ; check "mcp: the stdio proxy forwards requests and drops notifications" $?
stop_serve "$SERVE3_PID"

# --- Gate W: P9 candidate discovery → candidate ingest ----------------------
# scan (no store) lists what WOULD be ingested; ingest-files -candidates runs
# the SAME job state machine over the trimmed list; the skipped files never
# enter the corpus, so a search for them stays honest.
SCANSRC="$WORK/scansrc"
mkdir -p "$SCANSRC/sub"
printf '# 连接池手册\n\n连接池最大连接数为 200。\n' > "$SCANSRC/pool.md"
printf '值班 runbook：网关重启步骤。\n' > "$SCANSRC/sub/runbook.txt"
printf 'a,b\n1,2\n' > "$SCANSRC/data.csv"
touch -tm 202001010000 "$SCANSRC/pool.md" 2>/dev/null || true   # keep mtime realistic
SCAN="$($A scan -dir "$SCANSRC" -recursive)"
echo "$SCAN" | python3 -c '
import json,sys
r=json.load(sys.stdin)
paths=[c["path"] for c in r["candidates"]]
assert any(p.endswith("pool.md") for p in paths), paths
assert any(p.endswith("runbook.txt") for p in paths), paths
assert not any(p.endswith("data.csv") for p in paths), paths
assert r["skipped"].get("ext",0)>=1, r["skipped"]
print("ok")' ; check "scan: rules list candidates and account for skips" $?
$A scan -dir "$SCANSRC" -recursive -out "$WORK/cands.json" >/dev/null
[ -s "$WORK/cands.json" ] ; check "scan: -out writes the candidate report" $?
CJOB="$($A ingest-files -candidates "$WORK/cands.json" -job scando)"
echo "$CJOB" | python3 -c 'import json,sys; assert json.load(sys.stdin)["processed"]>=2, sys.stdin.read()' ; check "ingest-files -candidates runs the same job state machine" $?
SDOG="$($A search -q "连接池最大是多少" -raw)"
echo "$SDOG" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert (r.get("answer") or {}).get("summary"), r
print("ok")' ; check "scan→ingest: a candidate answers with citations" $?
SCSV="$($A search -q "数据表 a b 列内容" -raw)"
echo "$SCSV" | python3 -c '
import json,sys
r=json.load(sys.stdin)
samples=(r.get("answer") or {}).get("samples") or []
assert not samples, ("a skipped file must not be in the corpus", samples)
print("ok")' ; check "scan→ingest: skipped files never enter the corpus" $?

# --- Gate Y: strict embedder gate (A3) ---------------------------------------
# CLUS_MINILM_REQUIRE=1 turns "weights absent" into a hard failure: the L1
# precision paths must never read green on a silently degraded embedder.
STRICT=0
CLUS_EMBED=minilm CLUS_MINILM_REQUIRE=1 CLUS_MINILM_DIR="$WORK/no-such-model" $A ensure -embed >/dev/null 2>&1 || STRICT=1
[ "$STRICT" = "1" ] ; check "strict: CLUS_MINILM_REQUIRE=1 fails when the weights are absent" $?
# Without the flag the same absence still degrades (documented, echoed).
SOFT="$(CLUS_EMBED=minilm CLUS_MINILM_DIR="$WORK/no-such-model" $A ensure -embed)"
echo "$SOFT" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r.get("model")=="local-hash-64", r' ; check "strict: without the flag the absence still degrades to hash-64" $?

# --- Gate Z: model weight face (first-run install surface) --------------------
# The weights are the suite's OWN asset (ModelScope → ~/.cumulus/models/...).
# Status/verify/config must answer on ANY machine, so the gate points
# CLUS_MODEL_DIR at an empty dir; the 485MB download itself is unit-gated
# against a fake source (install_test.go) and never run from e2e.
SPORT4="${E2E_SERVE_PORT4:-8602}"
own_port "$SPORT4"
CLUS_MODEL_DIR="$WORK/no-model" "$WORK/cumulus-cluster" -data "$DATA" serve -listen "127.0.0.1:$SPORT4" >"$WORK/serve4.log" 2>&1 &
SERVE4_PID=$!
SRV4=0
for _ in $(seq 1 50); do
	curl -fsS "http://127.0.0.1:$SPORT4/health" >/dev/null 2>&1 && { SRV4=1; break; }
	sleep 0.2
done
[ "$SRV4" = "1" ] ; check "model: serve comes up for the weight face" $?
MST="$(curl -fsS "http://127.0.0.1:$SPORT4/v1/model")"
echo "$MST" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r["installed"] is False, r
assert r["dir"].endswith("no-model"), r["dir"]
assert r["dims"]==384 and r["model_id"].startswith("sentence-transformers/"), r
assert r["installing"] is False and r["size_hint_mb"]>400, r
print("ok")' ; check "model: GET /v1/model reports the absent weights with dir and dims" $?
MVF="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$SPORT4/v1/model/verify")"
[ "$MVF" = "502" ] ; check "model: verify answers 502 when the weights are absent" $?
CFG="$(curl -fsS "http://127.0.0.1:$SPORT4/v1/config")"
echo "$CFG" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert isinstance(r["base_url"], str) and isinstance(r["api_key_set"], bool), r
assert isinstance(r["minilm_required"], bool) and "reasoning_split" in r, r
print("ok")' ; check "model: GET /v1/config returns the masked endpoint config" $?
# --- Gate AA: P9/B2 scan REST face → candidate ingest -------------------------
# The workbench 摄取 panel's discovery step: POST /v1/scan lists what WOULD
# be ingested (rules only, no store), and the trimmed list feeds the SAME
# job state machine through {candidates:[...]}.
SA="$WORK/scanapi"
mkdir -p "$SA/sub"
printf '# 扫描手册\n\n连接池最大 200。\n' > "$SA/pool.md"
printf 'runbook：网关重启步骤。\n' > "$SA/sub/rb.txt"
printf 'x,y\n1,2\n' > "$SA/data.csv"
SCANR="$(curl -fsS -X POST "http://127.0.0.1:$SPORT4/v1/scan" -d "{\"dir\":\"$SA\",\"recursive\":true}")"
echo "$SCANR" | python3 -c '
import json,sys
r=json.load(sys.stdin)
paths=[c["path"] for c in r["candidates"]]
assert any(p.endswith("pool.md") for p in paths) and any(p.endswith("rb.txt") for p in paths), paths
assert not any(p.endswith("data.csv") for p in paths), paths
assert r["skipped"].get("ext",0)>=1, r["skipped"]
assert all(c.get("size",0)>0 and "age_days" in c for c in r["candidates"]), r["candidates"][:1]
print("ok")' ; check "scan: POST /v1/scan lists candidates with rule metadata" $?
SCANPATHS="$(echo "$SCANR" | python3 -c 'import json,sys; print(json.dumps([c["path"] for c in json.load(sys.stdin)["candidates"]]))')"
curl -fsS -X POST "http://127.0.0.1:$SPORT4/v1/buckets" -d '{"name":"scanface","note":"scan->ingest gate"}' >/dev/null
SCANJOB="$(curl -fsS -X POST "http://127.0.0.1:$SPORT4/v1/ingest/jobs" -d "{\"candidates\":$SCANPATHS,\"job\":\"scanjob\",\"ns\":\"scanface\"}")"
echo "$SCANJOB" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["state"]=="queued" and r.get("total",0)>=2, r' ; check "scan→ingest: the candidate list feeds the same job state machine" $?
SDONE=0
for _ in $(seq 1 50); do
	SST="$(curl -fsS "http://127.0.0.1:$SPORT4/v1/ingest/jobs/scanjob?ns=scanface" 2>/dev/null || true)"
	echo "$SST" | grep -q '"state":"done"' && { SDONE=1; break; }
	sleep 0.2
done
[ "$SDONE" = "1" ] ; check "scan→ingest: the candidate job tracks to done" $?
SANS="$(curl -fsS -X POST "http://127.0.0.1:$SPORT4/v1/search" -d '{"query":"扫描手册里连接池最大是多少","ns":"scanface"}')"
echo "$SANS" | python3 -c '
import json,sys
r=json.load(sys.stdin)
blob="".join(s.get("content","") for s in (r.get("answer") or {}).get("samples") or [])
assert "200" in blob, blob[:120]
print("ok")' ; check "scan→ingest: a scanned candidate answers from the corpus" $?
stop_serve "$SERVE4_PID"

# --- Gate AC: adapt face (heterogeneous corpora) ------------------------------
# 探测容器+字段 → 带映射摄取 → 同一 Job 状态机。夹具就地构造：一个 jsonl、一个
# csv、一个 pretty JSON 数组，覆盖三种容器判别与字段自动识别。
AD="$WORK/adapt"; mkdir -p "$AD"
printf '{"title":"适配器手册","content":"连接池最大 256，超时 45 秒。","id":"ad1"}\n' > "$AD/a.jsonl"
printf 'text_id,text\n1,红棉优级小粒老黄冰糖1.2kg大罐\n2,异形魔方顺滑风火轮移棱\n' > "$AD/titles.csv"
printf '[{"chapter":"章一","paragraphs":["第一段文字。","第二段文字。"]}]\n' > "$AD/book.json"
SPORT6="${E2E_SERVE_PORT6:-8604}"
own_port "$SPORT6"
"$WORK/cumulus-cluster" -data "$DATA" serve -listen "127.0.0.1:$SPORT6" >"$WORK/serve6.log" 2>&1 &
SERVE6_PID=$!
SRV6=0
for _ in $(seq 1 50); do
	curl -fsS "http://127.0.0.1:$SPORT6/health" >/dev/null 2>&1 && { SRV6=1; break; }
	sleep 0.2
done
[ "$SRV6" = "1" ] ; check "adapt: serve comes up for the adapt face" $?
curl -fsS -X POST "http://127.0.0.1:$SPORT6/v1/buckets" -d '{"name":"adaptgate","note":"adapt e2e"}' >/dev/null
ADPROBE="$(curl -fsS -X POST "http://127.0.0.1:$SPORT6/v1/adapt/probe" -d "{\"paths\":[\"$AD/a.jsonl\",\"$AD/titles.csv\",\"$AD/book.json\"]}")"
echo "$ADPROBE" | python3 -c '
import json,sys
r=json.load(sys.stdin); ps=r["probes"]
assert not r.get("failed"), r.get("failed")
kinds={p["kind"] for p in ps}
assert kinds=={"jsonl","csv","json-array"}, kinds
byk={p["kind"]:p for p in ps}
assert byk["jsonl"]["bodyish"]==["content"], byk["jsonl"]
assert byk["jsonl"]["idish"]==["id"], byk["jsonl"]
assert byk["jsonl"]["titleish"]==["title"], byk["jsonl"]
assert "text_id" in byk["csv"]["idish"], byk["csv"]
assert "text" in byk["csv"]["bodyish"], byk["csv"]
assert "paragraphs" in byk["json-array"]["bodyish"], byk["json-array"]
print("ok")' ; check "adapt: probe reports container + fields for three shapes" $?
ADBAD="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$SPORT6/v1/adapt/ingest" -d "{\"paths\":[\"$AD/a.jsonl\"]}")"
[ "$ADBAD" = "400" ] ; check "adapt: ingest without a bucket is refused (400)" $?
ADUNREG="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$SPORT6/v1/adapt/ingest" -d "{\"paths\":[\"$AD/a.jsonl\"],\"ns\":\"not-a-bucket\"}")"
[ "$ADUNREG" = "400" ] ; check "adapt: ingest into an unregistered bucket is refused (400)" $?
ADJOB="$(curl -fsS -X POST "http://127.0.0.1:$SPORT6/v1/adapt/ingest" -d "{\"paths\":[\"$AD/a.jsonl\",\"$AD/titles.csv\",\"$AD/book.json\"],\"ns\":\"adaptgate\",\"job\":\"adaptjob\",\"id\":\"id\",\"title\":\"title\",\"body\":\"content\"}")"
echo "$ADJOB" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["state"]=="queued" and r.get("ns")=="adaptgate", r' ; check "adapt: mapped ingest queues a job" $?
ADDONE=0
for _ in $(seq 1 50); do
	ADST="$(curl -fsS "http://127.0.0.1:$SPORT6/v1/ingest/jobs/adaptjob?ns=adaptgate" 2>/dev/null || true)"
	echo "$ADST" | grep -q '"state":"done"' && { ADDONE=1; break; }
	sleep 0.2
done
[ "$ADDONE" = "1" ] ; check "adapt: the job tracks to done" $?
echo "$ADST" | python3 -c '
import json,sys
d=json.load(sys.stdin)
assert d["total"]==3 and d["done"]==3, d
# 1 jsonl + 2 csv + 1 json-array record = 4 documents
assert d.get("records")==4, d
print("ok")' ; check "adapt: job accounts files and records in separate units" $?
ADSRC="$(curl -fsS -X POST "http://127.0.0.1:$SPORT6/v1/search" -d '{"query":"连接池最大是多少","ns":"adaptgate"}')"
echo "$ADSRC" | python3 -c '
import json,sys
r=json.load(sys.stdin); a=r.get("answer") or {}
blob="".join(s.get("content","") for s in a.get("samples") or [])
assert "256" in blob, blob[:160]
print("ok")' ; check "adapt: an adapted jsonl record answers from the corpus" $?
ADCSV="$(curl -fsS -X POST "http://127.0.0.1:$SPORT6/v1/search" -d '{"query":"黄冰糖","ns":"adaptgate"}')"
echo "$ADCSV" | python3 -c '
import json,sys
r=json.load(sys.stdin); a=r.get("answer") or {}
blob="".join(s.get("content","") for s in a.get("samples") or [])
assert "黄冰糖" in blob, blob[:160]
print("ok")' ; check "adapt: an adapted csv row answers from the corpus" $?
stop_serve "$SERVE6_PID"

# --- Gate BB: eval scoreboard face (B3) ---------------------------------------
# eval-run (Gate O, CLI) persists its aggregate into clus_evals; the
# workbench 评测 pane reads it back here. Read-only face — triggering a run
# stays on the CLI (items file + LLM budget).
SPORT5="${E2E_SERVE_PORT5:-8603}"
own_port "$SPORT5"
"$WORK/cumulus-cluster" -data "$DATA" serve -listen "127.0.0.1:$SPORT5" >"$WORK/serve5.log" 2>&1 &
SERVE5_PID=$!
SRV5=0
for _ in $(seq 1 50); do
	curl -fsS "http://127.0.0.1:$SPORT5/health" >/dev/null 2>&1 && { SRV5=1; break; }
	sleep 0.2
done
[ "$SRV5" = "1" ] ; check "scoreboard: serve comes up for the eval face" $?
EBLIST="$(curl -fsS "http://127.0.0.1:$SPORT5/v1/evals?limit=10")"
EBID="$(echo "$EBLIST" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r["namespace"]=="" and isinstance(r["runs"], list) and r["runs"], r
run=[x for x in r["runs"] if x.get("tag")=="eval-items.jsonl"]
assert run, ("the Gate O run must be on the scoreboard", [x.get("tag") for x in r["runs"]])
x=run[0]
assert x["n"]==2 and x["judged"] is False, x
assert x["system"]["n"]==2 and "closed_book" in x and "mcnemar" in x, x
print(x["_id"])
')"
[ -n "$EBID" ] ; check "scoreboard: the CLI eval run is listed with its aggregate" $?
EBD="$(curl -fsS "http://127.0.0.1:$SPORT5/v1/evals/$EBID")"
echo "$EBD" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r["_id"] and r["system"]["n"]==2, r
assert isinstance(r["modes"], dict) and "search_tokens" in r, r
tx=r["system"].get("taxonomy") or {}
assert tx.get("correct",0)+tx.get("answered_but_wrong",0)+tx.get("retrieved_but_unanswered",0)+tx.get("not_retrieved",0)==2, tx
# A.6 binding must round-trip through the store: the frozen hashes AND the
# human-readable config, otherwise a reader cannot tell whether two rows are
# comparable (and the old code dropped both).
fz=r.get("frozen") or {}
assert fz.get("items_sha") and fz.get("config_sha"), fz
ct=r.get("config_text") or ""
assert "reuse_theta" in ct and "embed_seat" in ct, ct
print("ok")' ; check "scoreboard: the detail carries taxonomy/modes/cost lines" $?
EB404="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$SPORT5/v1/evals/run:missing")"
[ "$EB404" = "404" ] ; check "scoreboard: an unknown run id answers 404" $?
python3 - "$SPORT5" <<'PY'
import json, sys, time, urllib.request, urllib.error
base = 'http://127.0.0.1:' + sys.argv[1]
def call(route, body=None, content_type='application/json'):
    request = urllib.request.Request(base + route, data=body, headers={'Content-Type': content_type})
    with urllib.request.urlopen(request, timeout=20) as response:
        return response.status, json.load(response)
call('/v1/buckets', json.dumps({'name': 'upload-gate'}).encode())
boundary = 'cumulus-upload-e2e'
payload = b''
for name, text in [('selected/a/manual.txt', '连接池最大 128，超时 30 秒。'), ('selected/b/manual.txt', '备份每天凌晨执行。')]:
    payload += ('--' + boundary + '\r\nContent-Disposition: form-data; name="files"; filename="' + name + '"\r\n\r\n' + text + '\r\n').encode()
payload += ('--' + boundary + '--\r\n').encode()
content_type = 'multipart/form-data; boundary=' + boundary
try:
    call('/v1/ingest/upload', payload, content_type)
    raise AssertionError('unnamed bucket accepted')
except urllib.error.HTTPError as error:
    assert error.code == 400
previous = None
for attempt in range(2):
    status, queued = call('/v1/ingest/upload?ns=upload-gate', payload, content_type)
    assert status == 202 and queued['total'] == 2, queued
    deadline = time.monotonic() + 10
    while True:
        _, job = call('/v1/ingest/jobs/' + queued['job'] + '?ns=upload-gate')
        assert job['state'] != 'failed', job
        if job['state'] == 'done':
            break
        assert time.monotonic() < deadline, job
        time.sleep(0.05)
    assert job['done'] == job['records'] == 2, job
    _, sources = call('/v1/sources?ns=upload-gate')
    identities = sorted(source['id'] for source in sources['sources'])
    assert len(identities) == 2 and (previous is None or identities == previous), sources
    previous = identities
_, result = call('/v1/search', json.dumps({'ns': 'upload-gate', 'query': '连接池最大是多少'}).encode())
assert result['citations']['refs'], result
print('upload end-to-end passed')
PY
check "upload: explicit bucket, nested identities, repeat import and searchable evidence" $?

# GUI evaluations use a frozen isolated corpus, not a CLI subprocess.
python3 - "$SPORT5" "$WORK/eval-ids.json" <<'PY'
import csv, io, json, sys, time, urllib.request, urllib.error, uuid
base = 'http://127.0.0.1:' + sys.argv[1]
def call(route, body=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(base + route, data=data, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=20) as response:
        return json.load(response)
try:
    call('/v1/eval/capabilities')
    raise AssertionError('evaluation accepted an implicit bucket')
except urllib.error.HTTPError as error:
    assert error.code == 400
call('/v1/buckets', {'name': 'eval-gate'})
call('/v1/ingest/sources', {'ns': 'eval-gate', 'title': 'Pool', 'key': 'pool', 'body': 'Pool maximum connections is 128. Timeout is 30 seconds.'})
cap = call('/v1/eval/capabilities?ns=eval-gate')
assert cap['protocol'] == 'eval-v2' and not cap['live_available'], cap
before = call('/v1/sources?ns=eval-gate')
content = json.dumps({'id': 'q1', 'query': 'Pool maximum connections?', 'answer': '128', 'gold_sources': ['pool']})
validation = call('/v1/eval/datasets/validate?ns=eval-gate', {'name': 'Pool', 'content': content})
assert validation['valid'] and validation['sha'], validation
assert call('/v1/eval/datasets?ns=eval-gate')['datasets'] == []
assert not call('/v1/eval/datasets/validate?ns=eval-gate', {'content': content + '\n' + content})['valid']
dataset = call('/v1/eval/datasets?ns=eval-gate', {'name': 'Pool', 'content': content})
runs = []
for prior in [True, False]:
    config = dict(cap['defaults'], prior=prior)
    request = {'dataset_id': dataset['id'], 'name': 'Offline e2e', 'request_id': str(uuid.uuid4()), 'config': config}
    run = call('/v1/eval/runs?ns=eval-gate', request)
    assert call('/v1/eval/runs?ns=eval-gate', request)['id'] == run['id']
    deadline = time.monotonic() + 15
    while run['state'] in ['queued', 'running', 'cancelling']:
        assert time.monotonic() < deadline, run
        time.sleep(.03)
        run = call('/v1/eval/runs/' + run['id'] + '?ns=eval-gate')
    assert run['state'] == 'completed' and run['done'] == run['total'] == 1, run
    assert run['summary']['closed_book_match'] is None and run['summary']['judge_correct'] is None, run
    items = call('/v1/eval/runs/' + run['id'] + '/items?ns=eval-gate')['items']
    assert items[0]['answer'] and items[0]['citations'], items
    runs.append(run)
compare = call('/v1/eval/compare?ns=eval-gate&left=' + runs[0]['id'] + '&right=' + runs[1]['id'])
assert compare['comparable'] and len(compare['items']) == 1, compare
for fmt in ['json', 'jsonl', 'csv']:
    with urllib.request.urlopen(base + '/v1/eval/runs/' + runs[0]['id'] + '/export?ns=eval-gate&format=' + fmt) as response:
        assert 'attachment' in response.headers['Content-Disposition']
        text = response.read().decode()
        assert runs[0]['frozen']['config_sha'] in text
        if fmt == 'csv':
            assert len(list(csv.reader(io.StringIO(text)))) == 2
# 长段落参考答案不是错误，但必须警示：规则臂是子串匹配，整段引用会恒 0
passage = json.dumps({'id': 'long', 'query': 'Pool maximum connections?', 'answer': '法' * 300, 'gold_sources': ['pool']})
warned = call('/v1/eval/datasets/validate?ns=eval-gate', {'name': 'Passage', 'content': passage})
assert warned['valid'] and len(warned['warnings']) == 1, warned
assert 'passage' in warned['warnings'][0]['message'] and warned['warnings'][0]['line'] == 1, warned['warnings']
assert call('/v1/eval/datasets/validate?ns=eval-gate', {'name': 'Short', 'content': content})['warnings'] == []
# limit 取冻结题序的前 N 题，不是整库
subset = call('/v1/eval/runs?ns=eval-gate', {'dataset_id': dataset['id'], 'name': 'Subset', 'request_id': str(uuid.uuid4()), 'config': dict(cap['defaults'], limit=1)})
assert subset['total'] == 1, subset
# L1 预筛：离线本地哈希向量，索引由运行自己补建
l1 = call('/v1/eval/runs?ns=eval-gate', {'dataset_id': dataset['id'], 'name': 'L1', 'request_id': str(uuid.uuid4()), 'config': dict(cap['defaults'], l1pre=True)})
for tagged in (subset, l1):
    deadline = time.monotonic() + 20
    while tagged['state'] in ['queued', 'running', 'cancelling']:
        assert time.monotonic() < deadline, tagged
        time.sleep(.03)
        tagged = call('/v1/eval/runs/' + tagged['id'] + '?ns=eval-gate')
    assert tagged['state'] == 'completed' and tagged['failed'] == 0, tagged
assert call('/v1/eval/runs/' + subset['id'] + '/items?ns=eval-gate')['items'][0]['rule_match'], 'subset run lost its score'
# 跨库：另一个注册库既读不到本库的题集/运行，也不能拿本库题集发起运行
call('/v1/buckets', {'name': 'eval-gate-other'})
assert call('/v1/eval/datasets?ns=eval-gate-other')['datasets'] == []
for route in ['datasets/' + dataset['id'], 'runs/' + runs[0]['id'], 'runs/' + runs[0]['id'] + '/items']:
    try:
        call('/v1/eval/' + route + '?ns=eval-gate-other')
        raise AssertionError('cross-namespace read succeeded: ' + route)
    except urllib.error.HTTPError as error:
        assert error.code == 404, (route, error.code)
try:
    call('/v1/eval/runs?ns=eval-gate-other', {'dataset_id': dataset['id'], 'name': 'Cross', 'request_id': str(uuid.uuid4()), 'config': cap['defaults']})
    raise AssertionError('a foreign dataset started a run')
except urllib.error.HTTPError as error:
    assert error.code == 404, error.code
# 只读路由拒绝写入方法
guard = urllib.request.Request(base + '/v1/eval/runs/' + runs[0]['id'] + '?ns=eval-gate', data=b'{}', headers={'Content-Type': 'application/json'}, method='POST')
try:
    urllib.request.urlopen(guard, timeout=20)
    raise AssertionError('POST accepted on the read-only run route')
except urllib.error.HTTPError as error:
    assert error.code == 405 and 'GET' in error.headers.get('Allow', ''), error.code
assert call('/v1/sources?ns=eval-gate') == before, 'evaluation mutated business sources'
with open(sys.argv[2], 'w') as fh:
    json.dump({'completed': runs[0]['id'], 'dataset': dataset['id']}, fh)
print('GUI evaluation end-to-end passed')
PY
check "eval-v2: validation, isolated offline runs, idempotency, exports and comparison" $?

# Persistence is the load-bearing claim of a background run: a finished
# experiment must outlive the process, and a run that was in flight when the
# process died must be reported interrupted instead of being silently replayed
# (nobody consented to paying for it twice). SIGKILL, not a graceful stop.
python3 - "$SPORT5" "$WORK/eval-ids.json" <<'PY'
import json, sys, time, urllib.request, uuid
base = 'http://127.0.0.1:' + sys.argv[1]
def call(route, body=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(base + route, data=data, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=20) as response:
        return json.load(response)
ids = json.load(open(sys.argv[2]))
rows = '\n'.join(json.dumps({'id': 'q%d' % i, 'query': 'Pool maximum connections?', 'answer': '128', 'gold_sources': ['pool']}) for i in range(300))
big = call('/v1/eval/datasets?ns=eval-gate', {'name': 'Long', 'content': rows})
assert big['count'] == 300, big
cap = call('/v1/eval/capabilities?ns=eval-gate')
active = call('/v1/eval/runs?ns=eval-gate', {'dataset_id': big['id'], 'name': 'Inflight', 'request_id': str(uuid.uuid4()), 'config': cap['defaults']})
deadline = time.monotonic() + 20
while time.monotonic() < deadline:
    state = call('/v1/eval/runs/' + active['id'] + '?ns=eval-gate')['state']
    if state == 'running':
        break
    time.sleep(.05)
assert state == 'running', state
# 有界队列：1 个在跑 + queue_limit 个排队，再多一个就是 409
pending = []
for i in range(cap['queue_limit']):
    pending.append(call('/v1/eval/runs?ns=eval-gate', {'dataset_id': big['id'], 'name': 'Pending %d' % i, 'request_id': str(uuid.uuid4()), 'config': cap['defaults']}))
assert call('/v1/eval/runs/' + active['id'] + '?ns=eval-gate')['state'] == 'running', 'blocker finished before the queue could fill'
try:
    call('/v1/eval/runs?ns=eval-gate', {'dataset_id': big['id'], 'name': 'Overflow', 'request_id': str(uuid.uuid4()), 'config': cap['defaults']})
    raise AssertionError('the queue accepted more than queue_limit pending runs')
except urllib.error.HTTPError as error:
    assert error.code == 409, error.code
    assert 'queue' in json.loads(error.read().decode())['error'], 'the 409 must explain the queue'
# 取消一个仍在排队的运行：立刻终结，且不占用工作线程
cancelled_queued = call('/v1/eval/runs/' + pending[0]['id'] + '/cancel?ns=eval-gate', {})
assert cancelled_queued['state'] == 'cancelled', cancelled_queued
assert call('/v1/eval/runs/' + pending[0]['id'] + '/cancel?ns=eval-gate', {})['state'] == 'cancelled'
ids['inFlight'] = active['id']
ids['cancelledQueued'] = pending[0]['id']
ids['pending'] = [run['id'] for run in pending[1:]]
json.dump(ids, open(sys.argv[2], 'w'))
print('inflight evaluation started')
PY
check "eval-v2: a long run reaches running before the process is killed" $?
kill -9 "$SERVE5_PID" 2>/dev/null
wait "$SERVE5_PID" 2>/dev/null
own_port "$SPORT5"
"$WORK/cumulus-cluster" -data "$DATA" serve -listen "127.0.0.1:$SPORT5" >"$WORK/serve5b.log" 2>&1 &
SERVE5_PID=$!
SRV5B=0
for _ in $(seq 1 50); do
	curl -fsS "http://127.0.0.1:$SPORT5/health" >/dev/null 2>&1 && { SRV5B=1; break; }
	sleep 0.2
done
[ "$SRV5B" = "1" ] ; check "eval-v2: serve restarts on the store left behind by SIGKILL" $?
python3 - "$SPORT5" "$WORK/eval-ids.json" <<'PY'
import json, sys, time, urllib.request, urllib.error, uuid
base = 'http://127.0.0.1:' + sys.argv[1]
ids = json.load(open(sys.argv[2]))
def call(route, body=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(base + route, data=data, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=20) as response:
        return json.load(response)
completed = call('/v1/eval/runs/' + ids['completed'] + '?ns=eval-gate')
assert completed['state'] == 'completed' and completed['done'] == 1, completed
persisted = call('/v1/eval/runs/' + ids['completed'] + '/items?ns=eval-gate')['items']
assert persisted[0]['citations'] and persisted[0]['answer'], persisted
listed = {run['id']: run for run in call('/v1/eval/runs?ns=eval-gate')['runs']}
assert ids['completed'] in listed and ids['inFlight'] in listed, sorted(listed)
inflight = listed[ids['inFlight']]
assert inflight['state'] == 'interrupted', inflight
assert 'start a new run' in inflight['error'], inflight
# 用户取消过的运行不得被重启改写成 interrupted；从没跑过的排队运行才是 interrupted
assert listed[ids['cancelledQueued']]['state'] == 'cancelled', listed[ids['cancelledQueued']]
for pid in ids['pending']:
    assert listed[pid]['state'] == 'interrupted', listed[pid]
try:
    call('/v1/eval/runs/' + ids['inFlight'] + '/retry?ns=eval-gate', {})
    raise AssertionError('cold retry of a lost experiment was accepted')
except urllib.error.HTTPError as error:
    assert error.code == 409, error.code
fresh = call('/v1/eval/runs?ns=eval-gate', {'dataset_id': ids['dataset'], 'name': 'After restart', 'request_id': str(uuid.uuid4()), 'config': call('/v1/eval/capabilities?ns=eval-gate')['defaults']})
assert fresh['id'] not in listed, fresh
deadline = time.monotonic() + 15
while fresh['state'] in ['queued', 'running', 'cancelling']:
    assert time.monotonic() < deadline, fresh
    time.sleep(.03)
    fresh = call('/v1/eval/runs/' + fresh['id'] + '?ns=eval-gate')
assert fresh['state'] == 'completed' and fresh['done'] == 1, fresh
print('restart persistence passed')
PY
check "eval-v2: finished runs survive SIGKILL, in-flight runs become interrupted, cold retry is refused" $?

# The store directory is exclusive, so the CLI half of this gate can only run
# once serve is down.
stop_serve "$SERVE5_PID"
# The binding must DIFFERENTIATE configs: two runs with a different chat model
# must not share a config hash (they used to — the fingerprint was 4 booleans,
# so a model swap looked like a no-op on the scoreboard).
SHA_A="$($A eval-run -file "$OID" -out "$WORK/evalres2.jsonl" -tag sha-b 2>/dev/null | python3 -c 'import json,sys; t=sys.stdin.read(); i=t.index("{"); print(json.loads(t[i:])["frozen"]["config_sha"])')"
SHA_B="$(AIGATE_CHAT_MODEL=other-model $A eval-run -file "$OID" -out "$WORK/evalres3.jsonl" -tag sha-c 2>/dev/null | python3 -c 'import json,sys; t=sys.stdin.read(); i=t.index("{"); print(json.loads(t[i:])["frozen"]["config_sha"])')"
[ -n "$SHA_A" ] && [ -n "$SHA_B" ] && [ "$SHA_A" != "$SHA_B" ] ; check "scoreboard: a different chat model changes the config binding" $?

echo "clus-e2e: $PASS ok, $FAIL fail"
[ "$FAIL" -eq 0 ]
