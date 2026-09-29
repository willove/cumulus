#!/usr/bin/env bash
# stop_reason distribution probe (Jev-Mem roadmap step 1, 2026-09-29).
#
# One arm, frozen realeval-30 legal set (shared with semhead-ab so the probe
# is comparable with archived arms), default config. NO -prior: the closed
# book arm has no retrieval loop, so it has no stop_reason to contribute.
# -judge stays on so the exit histogram cross-tabs against correctness.
#
# What this decides (jev-mem-budget-direction memory): sufficient-heavy →
# three-way conjunction + batched scoring (v1.5/v3) pay first; budget-heavy →
# budget allocation (v2) pays first; utility-heavy → the pessimistic exit is
# load-bearing and its thresholds deserve calibration before anything else.
#
# Usage: scripts/stopreason-probe.sh [LIMIT]
set -eu
cd "$(dirname "$0")/.."

N="${1:-30}"
BASE="$(pwd)/var/stopreason-probe"
SRC="$(pwd)/var/semhead-ab"
ask="$BASE/cumulus-cluster"
state="$BASE/arm"
mkdir -p "$BASE"
go build -o "$ask" ./cmd/cumulus-cluster

# Embedder seat must match the frozen corpus embeddings (paired-ab.sh rule).
export CLUS_EMBED=minilm

have=0
[ -s "$state/results.jsonl" ] && have="$(wc -l < "$state/results.jsonl" | tr -d ' ')"
if [ "$have" -lt "$N" ]; then
  if [ ! -d "$state/data" ]; then
    mkdir -p "$state"
    "$ask" -data "$state/data" ensure
    "$ask" -data "$state/data" ingest-jsonl -file "$SRC/corpus.jsonl" -job probe
    "$ask" -data "$state/data" ensure -embed >/dev/null
    echo "probe: fresh store built (docs=$(wc -l < "$SRC/corpus.jsonl" | tr -d ' '))"
  else
    echo "probe: resuming $have/$N"
  fi
  "$ask" -data "$state/data" eval-run \
    -file "$SRC/items.jsonl" -out "$state/results.jsonl" \
    -judge -tag stopreason-probe -limit "$((N - have))" > "$state/report.json"
else
  echo "probe: already complete ($have/$N)"
fi
python3 scripts/stopreason_tally.py < "$state/results.jsonl"
