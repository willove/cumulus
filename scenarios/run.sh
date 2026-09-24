#!/usr/bin/env bash
# Scenario runner (design §5 案例语料): ingest the scenario corpus into a
# throwaway embedded store, run its queries against the real path, assert
# expectations.
# Usage: bash scenarios/run.sh [name ...]   (default: all)
set -u
cd "$(dirname "$0")/.."
# Scenario runs are stub-mode too: isolate from the developer's environment.
# Shared with e2e.sh so the two gates cannot drift apart again — before this,
# scenarios went 13 ok -> 4 ok / 9 fail under an exported LLM_BASE_URL while
# e2e.sh was already fixed, because each had its own half of the isolation.
# shellcheck source=scripts/offline-gate.sh
. "$(cd "$(dirname "$0")/.." && pwd)/scripts/offline-gate.sh"
WORK="$(mktemp -d)"
STORE="$WORK/data"
PASS=0
FAIL=0
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

go build -o "$WORK/cumulus-cluster" ./cmd/cumulus-cluster || { echo "scenario: FAIL building cumulus-cluster"; exit 1; }
"$WORK/cumulus-cluster" -data "$STORE" ensure >/dev/null

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
ccl = os.environ["CLUS"]; store = os.environ["STORE"]
npass = nfail = 0
for doc in scn.get("corpus", []):
    args = [ccl, "-data", store, "put", "-title", doc.get("title", doc["key"]),
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
    r = subprocess.run([ccl, "-data", store, "search", "-q", q["q"], "-raw"],
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

export CLUS="$WORK/cumulus-cluster"
export STORE
if [ "$#" -gt 0 ]; then
	for s in "$@"; do run_scenario "scenarios/$s"; done
else
	for d in scenarios/*/; do run_scenario "${d%/}"; done
fi
echo "scenarios: $PASS ok, $FAIL fail"
[ "$FAIL" -eq 0 ]
