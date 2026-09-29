#!/usr/bin/env bash
# Generic paired endpoint-tier A/B: one env var, two values, one frozen item set.
#
# Why a script and not run.py (perf-plan §4.8): run.py always passes -l1pre, so
# an ablation of the DEFAULT configuration is not expressible there. This runs
# the production default on both arms and changes exactly one variable.
#
# Discipline (design-plan §6.5):
#   - a FRESH store per arm, so no cluster/cites/evidence carryover
#     (the confound that made baike-baseline2 reuse arm 1's clusters and drift
#     the mode mix FAST 9 -> 20)
#   - identical frozen items, -judge -prior, same embedder seat
#   - record-only: writes under var/, never a threshold or a config value
#   - RESUMABLE, with the correction that cost a wasted arm once:
#     `eval-run -limit N` caps THIS invocation's items, not the file's length,
#     so a killed run leaves k rows and a naive re-run with -limit N yields
#     k+N. The limit here is always the shortfall against the target, and the
#     summary compares only the INTERSECTION of item ids.
#
# Usage:
#   scripts/paired-ab.sh CLUS_ADMIT_SEMANTIC_HEAD 0 4 semhead   [LIMIT]
#   scripts/paired-ab.sh CLUS_DECOMPOSE            0 1 decompose [LIMIT]
set -eu
cd "$(dirname "$0")/.."

[ $# -ge 3 ] || { sed -n '2,25p' "$0"; exit 2; }
VAR="$1"; VAL0="$2"; VAL1="$3"; TAG="$4"
N="${5:-12}"

BASE="$(pwd)/var/ab-$TAG"
ask="$BASE/cumulus-cluster"
mkdir -p "$BASE"
go build -o "$ask" ./cmd/cumulus-cluster

# The embedder seat must match the corpus embedding, or the comparison measures
# embedder mismatch rather than the variable under test.
export CLUS_EMBED=minilm

# One frozen item set + corpus, built once and shared by every arm of this tag.
# Deterministic: realeval.sh prep draws the same first N triples from the same
# dataset, so a later re-run reproduces the same questions.
if [ ! -s "$BASE/items.jsonl" ]; then
	CNLAW_DIR="${CNLAW_DIR:-$HOME/datasets/cn-law-rag/finetune_dataset.jsonl}" \
		REALEVAL_N=30 scripts/realeval.sh prep
	cp var/realeval/items.jsonl "$BASE/items.jsonl"
	cp var/realeval/corpus.jsonl "$BASE/corpus.jsonl"
	rm -rf var/realeval
	echo "ab: frozen items=$(wc -l < "$BASE/items.jsonl" | tr -d ' ') docs=$(wc -l < "$BASE/corpus.jsonl" | tr -d ' ')"
fi

run_arm() { # armname value
	local name="$1" val="$2"
	local state="$BASE/$name"
	local have=0
	[ -s "$state/results.jsonl" ] && have="$(wc -l < "$state/results.jsonl" | tr -d ' ')"
	if [ "$have" -ge "$N" ]; then
		echo "ab: $name already complete ($have/$N), skipping"
		return 0
	fi
	if [ ! -d "$state/data" ]; then
		mkdir -p "$state"
		"$ask" -data "$state/data" ensure
		"$ask" -data "$state/data" ingest-jsonl -file "$BASE/corpus.jsonl" -job ab
		"$ask" -data "$state/data" ensure -embed >/dev/null
		echo "ab: $name fresh store built"
	else
		echo "ab: $name resuming $have/$N"
	fi
	echo "ab: $name — $VAR=$val"
	env "$VAR=$val" "$ask" -data "$state/data" eval-run \
		-file "$BASE/items.jsonl" -out "$state/results.jsonl" \
		-judge -prior -tag "$TAG-$name" -limit "$((N - have))" > "$state/report.json"
	echo "ab: $name report written"
}

run_arm a0 "$VAL0"
run_arm a1 "$VAL1"

AB_BASE="$BASE" AB_TAG="$TAG" AB_VAR="$VAR" AB_VALS="$VAL0/$VAL1" \
	python3 scripts/paired_ab_summary.py
