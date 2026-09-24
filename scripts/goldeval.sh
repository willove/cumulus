#!/usr/bin/env bash
# Golden-set scoreboard runner (R-E2, lens-notes §7.4): verify checksums,
# then run eval-run against a prepared store. Usage:
#   scripts/goldeval.sh chinalaw39|cnlaw30     (default chinalaw39)
# The store follows the set — build it first with `scripts/chinalaw-prep.sh
# ingest` (chinalaw sets) or `scripts/realeval.sh prep` (cnlaw30).
# Env: STORE (store directory override), EXTRA (eval-run flags, default
#      "-judge -prior"), LIMIT (batch size), OUT (results jsonl; resumable).
set -u
cd "$(dirname "$0")/.."
SET="${1:-chinalaw39}"
ITEMS="testdata/eval/$SET.jsonl"
[ -f "$ITEMS" ] || { echo "goldeval: unknown set $SET"; exit 1; }
shasum -a 256 -c testdata/eval/MANIFEST.sha256 || { echo "goldeval: golden set drifted"; exit 1; }
case "$SET" in
chinalaw39) STORE="${STORE:-var/chinalaw/data}" ;;
chinalaw158) STORE="${STORE:-var/chinalaw/data}" ;;  # 规模化集（显著性复测）；RESUME：LIMIT 分批跑同一 OUT
cnlaw30) STORE="${STORE:-var/realeval/data}" ;;
*) echo "goldeval: no default store for $SET"; exit 1 ;;
esac
[ -d "$STORE" ] || { echo "goldeval: store $STORE not found — build it first (chinalaw-prep.sh ingest / realeval.sh prep)"; exit 1; }
OUT="${OUT:-var/goldeval-$SET-results.jsonl}"
mkdir -p "$(dirname "$OUT")"
go build -o var/goldeval-clus ./cmd/cumulus-cluster || exit 1
"$PWD/var/goldeval-clus" -data "$STORE" eval-run -file "$ITEMS" -out "$OUT" \
	${EXTRA:--judge -prior} -limit "${LIMIT:-0}"