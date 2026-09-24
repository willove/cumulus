#!/usr/bin/env bash
# Rebuild the Chinese_Law (or any one-line-per-article statute dir) corpus and
# optionally ingest it. Keys follow scripts/chinalaw_corpus.py so gold_sources
# in testdata/eval/chinalaw39.jsonl stay valid.
#
# Usage:
#   scripts/chinalaw-prep.sh              # build var/chinalaw/corpus.jsonl only
#   scripts/chinalaw-prep.sh ingest       # build + ensure + ingest-jsonl
# Env:
#   LAW_DIR   default ~/datasets/KuugoRen/Chinese_Law
#   JOB       default chinalaw
set -euo pipefail
cd "$(dirname "$0")/.."
LAW_DIR="${LAW_DIR:-$HOME/datasets/KuugoRen/Chinese_Law}"
OUT="${OUT:-var/chinalaw/corpus.jsonl}"
JOB="${JOB:-chinalaw}"
STATE="$(pwd)/var/chinalaw"
STORE="$STATE/data"
mkdir -p "$STATE"

[ -d "$LAW_DIR" ] || { echo "chinalaw-prep: LAW_DIR not found: $LAW_DIR" >&2; exit 1; }
python3 scripts/chinalaw_corpus.py --dir "$LAW_DIR" --out "$OUT"

if [ "${1:-}" != "ingest" ]; then
	echo "chinalaw-prep: wrote $OUT (pass 'ingest' to load it into the store at $STORE)"
	exit 0
fi

# One embedded store directory — no server process. Badger locks it, so no
# other ask/cumulite process may hold it while this runs.
go build -o "$STATE/ask" ./cmd/ask || exit 1
"$STATE/ask" -data "$STORE" ensure
"$STATE/ask" -data "$STORE" ingest-jsonl -file "$OUT" -job "$JOB"
echo "chinalaw-prep: ingested $OUT → $STORE job=$JOB"
