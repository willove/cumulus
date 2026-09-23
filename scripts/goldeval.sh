#!/usr/bin/env bash
# Golden-set scoreboard runner (R-E2, lens-notes §7.4): verify checksums,
# then run eval-run against a running cumudb. Usage:
#   scripts/goldeval.sh chinalaw39|cnlaw30     (default chinalaw39)
# Env: SERVER (default :8594 for chinalaw / :8593 for cnlaw),
#      EXTRA (eval-run flags, default "-judge -prior"), LIMIT (batch size),
#      OUT (results jsonl; resumable).
set -u
cd "$(dirname "$0")/.."
SET="${1:-chinalaw39}"
ITEMS="testdata/eval/$SET.jsonl"
[ -f "$ITEMS" ] || { echo "goldeval: unknown set $SET"; exit 1; }
shasum -a 256 -c testdata/eval/MANIFEST.sha256 || { echo "goldeval: golden set drifted"; exit 1; }
case "$SET" in
chinalaw39) SERVER="${SERVER:-http://127.0.0.1:8594}" ;;
chinalaw158) SERVER="${SERVER:-http://127.0.0.1:8594}" ;;  # 规模化集（显著性复测）；RESUME：LIMIT 分批跑同一 OUT
cnlaw30) SERVER="${SERVER:-http://127.0.0.1:8593}" ;;
*) echo "goldeval: no default server for $SET"; exit 1 ;;
esac
OUT="${OUT:-var/goldeval-$SET-results.jsonl}"
mkdir -p "$(dirname "$OUT")"
go build -o var/goldeval-ask ./cmd/ask || exit 1
"$PWD/var/goldeval-ask" -server "$SERVER" eval-run -file "$ITEMS" -out "$OUT" \
	${EXTRA:--judge -prior} -limit "${LIMIT:-0}"
