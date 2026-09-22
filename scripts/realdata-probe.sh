#!/usr/bin/env bash
# Real-corpus probe (R3/R5 extension): adversarial evaluation against the
# operator's local datasets (~/datasets, outside any repo).
#
# cn-law-rag (Apache-2.0): 65,783 (anchor 口语问, positive 法条, negative 难负例)
# triples. We take a sample, ingest positives+negatives as the corpus, ask the
# anchors, and report how often the evidence lands on the POSITIVE statute —
# with its hard negative sitting right next to it in the corpus. This is the
# B5 (arms) / localization quality measurement on real text.
#
# Recorded, not gated. Skips gracefully when the dataset is absent.
# Usage: bash scripts/realdata-probe.sh [N]   (default 40 triples)
set -u
cd "$(dirname "$0")/.."
DATA="${CNLAW_DIR:-$HOME/datasets/cn-law-rag/finetune_dataset.jsonl}"
N="${1:-40}"
if [ ! -f "$DATA" ]; then
	echo "realdata-probe: $DATA not found — skip (record-only probe)"
	exit 0
fi

WORK="$(mktemp -d)"
DB_PID=""
trap 'if [ -n "$DB_PID" ] && [ "$DB_PID" -eq "$DB_PID" ] 2>/dev/null; then kill "$DB_PID" 2>/dev/null; fi; rm -rf "$WORK"' EXIT
DB_PORT=8592

(cd ../../cumudb && go build -o "$WORK/cumudb" ./cmd/cumudb)
go build -o "$WORK/ask" ./cmd/ask || exit 1
"$WORK/cumudb" -listen "127.0.0.1:$DB_PORT" -data "$WORK/data" -log-level warn >"$WORK/cumudb.log" 2>&1 &
DB_PID=$!
for _ in $(seq 1 50); do
	kill -0 "$DB_PID" 2>/dev/null || { echo "realdata-probe: cumudb died"; exit 1; }
	curl -fsS "http://127.0.0.1:$DB_PORT/v1/health" >/dev/null 2>&1 && break
	sleep 0.2
done
"$WORK/ask" -server "http://127.0.0.1:$DB_PORT" ensure >/dev/null

# Sample N triples; positives AND negatives both enter the corpus (hard
# negatives are the point). Keys: law<k> / neg<k>.
python3 - "$DATA" "$N" "$WORK" <<'PY'
import json, sys
src, n, work = sys.argv[1], int(sys.argv[2]), sys.argv[3]
qs, docs, seen = [], {}, set()
with open(src, encoding="utf-8") as f:
    for line in f:
        if len(qs) >= n:
            break
        try:
            t = json.loads(line)
        except Exception:
            continue
        a, pos, neg = t.get("anchor"), t.get("positive"), t.get("negative")
        if not (a and pos and neg):
            continue
        k = len(qs)
        if pos not in seen:
            docs["law%03d" % k] = pos
            seen.add(pos)
        docs["neg%03d" % k] = neg
        qs.append({"q": a, "gold": "law%03d" % k})
with open(work + "/queries.json", "w", encoding="utf-8") as f:
    json.dump(qs, f, ensure_ascii=False)
with open(work + "/corpus.jsonl", "w", encoding="utf-8") as f:
    for key, text in docs.items():
        f.write(json.dumps({"key": key, "title": key, "text": text}, ensure_ascii=False) + "\n")
print("corpus docs=%d queries=%d" % (len(docs), len(qs)))
PY

A=("$WORK/ask" -server "http://127.0.0.1:$DB_PORT")
export ASK="$WORK/ask"
export SRV="http://127.0.0.1:$DB_PORT"
"${A[@]}" ingest-jsonl -file "$WORK/corpus.jsonl" -job cnlaw >/dev/null

python3 - "$WORK/queries.json" <<'PY'
import json, os, subprocess, sys
ask = os.environ["ASK"]; srv = os.environ["SRV"]
qs = json.load(open(sys.argv[1], encoding="utf-8"))
hit = cite = n = 0
for item in qs:
    r = subprocess.run([ask, "-server", srv, "search", "-q", item["q"], "-raw"],
                       capture_output=True, text=True)
    try:
        res = json.loads(r.stdout)
    except Exception:
        continue
    n += 1
    gold = item["gold"]
    blob = json.dumps(res, ensure_ascii=False)
    if gold in blob:
        hit += 1
    refs = (res.get("citations") or {}).get("refs") or []
    if any(ref.get("source_id", "").endswith(gold) or gold in (ref.get("source_id") or "") for ref in refs):
        cite += 1
print("REALDATA cn-law: gold-mentioned %d/%d · cited-gold %d/%d (hard negatives in corpus)" % (hit, n, cite, n))
PY
echo "realdata-probe done — recorded, not gated"
