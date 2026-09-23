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
#   DB_PORT   default 8594 (chinalaw goldeval port)
#   JOB       default chinalaw
set -euo pipefail
cd "$(dirname "$0")/.."
LAW_DIR="${LAW_DIR:-$HOME/datasets/KuugoRen/Chinese_Law}"
OUT="${OUT:-var/chinalaw/corpus.jsonl}"
DB_PORT="${DB_PORT:-8594}"
JOB="${JOB:-chinalaw}"
SRV="http://127.0.0.1:$DB_PORT"
STATE="$(pwd)/var/chinalaw"
mkdir -p "$STATE"

[ -d "$LAW_DIR" ] || { echo "chinalaw-prep: LAW_DIR not found: $LAW_DIR" >&2; exit 1; }
python3 scripts/chinalaw_corpus.py --dir "$LAW_DIR" --out "$OUT"

if [ "${1:-}" != "ingest" ]; then
	echo "chinalaw-prep: wrote $OUT (pass 'ingest' to load into cumudb :$DB_PORT)"
	exit 0
fi

db_up() {
	if curl -fsS "$SRV/v1/health" >/dev/null 2>&1; then return 0; fi
	(cd ../db-works/cumudb && go build -o "$STATE/cumudb" ./cmd/cumudb) || exit 1
	go build -o "$STATE/ask" ./cmd/ask || exit 1
	"$STATE/cumudb" -listen "127.0.0.1:$DB_PORT" -data "$STATE/data" -log-level warn >>"$STATE/db.log" 2>&1 &
	for _ in $(seq 1 50); do
		curl -fsS "$SRV/v1/health" >/dev/null 2>&1 && return 0
		sleep 0.2
	done
	echo "chinalaw-prep: cumudb failed" >&2
	exit 1
}
db_up
go build -o "$STATE/ask" ./cmd/ask || exit 1
"$STATE/ask" -server "$SRV" ensure
"$STATE/ask" -server "$SRV" ingest-jsonl -file "$OUT" -job "$JOB"
echo "chinalaw-prep: ingested $OUT → $SRV job=$JOB"
