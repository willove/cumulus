#!/usr/bin/env bash
# Scenario runner (design §5 案例语料): boot a temp cumudb, ingest the
# scenario corpus, run its queries against the real path, assert expectations.
# Usage: bash scenarios/run.sh [name ...]   (default: all)
set -u
cd "$(dirname "$0")/.."
# Scenario runs are stub-mode too: isolate from the developer's .env.
export ASK_ENV=/dev/null
WORK="$(mktemp -d)"
DB_PID=""
PASS=0
FAIL=0
DB_PORT="${E2E_DB_PORT:-8597}"
cleanup() {
	if [ -n "$DB_PID" ] && [ "$DB_PID" -eq "$DB_PID" ] 2>/dev/null; then kill "$DB_PID" 2>/dev/null; fi
	rm -rf "$WORK"
}
trap cleanup EXIT

(cd ../db-works/cumudb && go build -o "$WORK/cumudb" ./cmd/cumudb && go build -o "$WORK/cumuctl" ./cmd/cumuctl) || { echo "scenario: FAIL building cumudb"; exit 1; }
go build -o "$WORK/ask" ./cmd/ask || { echo "scenario: FAIL building ask"; exit 1; }
"$WORK/cumudb" -listen "127.0.0.1:$DB_PORT" -data "$WORK/data" -log-level warn >"$WORK/cumudb.log" 2>&1 &
DB_PID=$!
for _ in $(seq 1 50); do
	kill -0 "$DB_PID" 2>/dev/null || { echo "scenario: FAIL cumudb died"; tail -5 "$WORK/cumudb.log"; exit 1; }
	curl -fsS "http://127.0.0.1:$DB_PORT/v1/health" >/dev/null 2>&1 && break
	sleep 0.2
done
"$WORK/cumuctl" -server "http://127.0.0.1:$DB_PORT" coll create ask_sources >/dev/null
"$WORK/cumuctl" -server "http://127.0.0.1:$DB_PORT" coll create ask_evidence >/dev/null
"$WORK/ask" -server "http://127.0.0.1:$DB_PORT" ensure >/dev/null
A="$WORK/ask -server http://127.0.0.1:$DB_PORT"

run_scenario() {
	dir="$1"
	name="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["name"])' "$dir/scenario.json")"
	if [ "$#" -ge 2 ]; then
		shift
		for want in "$@"; do [ "$want" = "$name" ] || return 0; done
	fi
	printf 'scenario %s (%s)\n' "$name" "$dir"
	python3 - "$dir" <<'PY'
import json, os, subprocess, sys
scn = json.load(open(sys.argv[1] + "/scenario.json"))
ask = os.environ["ASK"]; srv = os.environ["SRV"]
npass = nfail = 0
for doc in scn.get("corpus", []):
    args = [ask, "-server", srv, "put", "-title", doc.get("title", doc["key"]),
            "-key", doc["key"], "-type", "md", "-body-file", sys.argv[1] + "/" + doc["file"]]
    r = subprocess.run(args, capture_output=True, text=True)
    ok_create = '"status"' in r.stdout
    if ok_create:
        print("  ok: corpus ingest %s" % doc["key"])
    else:
        print("  FAIL: corpus ingest %s: %s %s" % (doc["key"], r.stdout[:120], r.stderr[:120]))
    if ok_create: npass += 1
    else: nfail += 1
for q in scn.get("queries", []):
    r = subprocess.run([ask, "-server", srv, "search", "-q", q["q"], "-raw"],
                       capture_output=True, text=True)
    blob = r.stdout
    reasons = []
    for needle in q.get("must_contain", []):
        if needle not in blob:
            reasons.append("missing %r" % needle)
    if "expect_mode" in q:
        try:
            got = json.loads(blob).get("mode")
            if got != q["expect_mode"]:
                reasons.append("mode=%r want %r" % (got, q["expect_mode"]))
        except Exception as e:
            reasons.append("unparseable output: %s" % e)
    if reasons:
        nfail += 1
        print("  FAIL: %-24s %s" % (q["q"], "; ".join(reasons)))
    else:
        npass += 1
        print("  ok: %s" % q["q"])
print("scenario %s: %d ok, %d fail" % (scn["name"], npass, nfail))
sys.exit(0 if nfail == 0 else 1)
PY
	if [ $? -ne 0 ]; then
		FAIL=$((FAIL + 1))
		echo "  --- scenario $name FAILED"
		return 1
	fi
	PASS=$((PASS + 1))
}

export ASK="$WORK/ask"
export SRV="http://127.0.0.1:$DB_PORT"
if [ "$#" -gt 0 ]; then
	for s in "$@"; do run_scenario "scenarios/$s"; done
else
	for d in scenarios/*/; do run_scenario "${d%/}"; done
fi
echo "scenarios: $PASS ok, $FAIL fail"
[ "$FAIL" -eq 0 ]
