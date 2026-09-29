#!/usr/bin/env bash
# P1-3 paired A/B: does giving the semantic arm the HEAD of the DEEP admission
# list reproduce the -l1pre gain without -l1pre?
#
# Why this arm shape (perf-plan §4.2):
#   rankFunc appends the KNN arm after a wide lexical sweep, and both
#   usageFirst and mixedAffinity cut the list to maxDeepLoops — so a document
#   the embedder ranked #1 by cosine lands at position ~500 and is discarded
#   before the DEEP loop sees it. -l1pre measured that fixing the ORDER is worth
#   Ev.Rec 30.0%→43.3% / EM 43.3%→66.7% on the same 30 cn-law anchors.
#
#   run.py cannot host this A/B: it always passes -l1pre, which replaces the
#   candidate list with the KNN hits — the semantic arm is then already in
#   order, so the marginal effect here would be near zero. This script runs the
#   PRODUCTION DEFAULT (no -l1pre) on both arms, which is where the defect is.
#
# Discipline (design-plan §6.5):
#   - one FRESH store per arm → no cluster/cites/evidence carryover, which is
#     the confound that made baike-baseline2 reuse arm 1's clusters
#   - both arms: identical frozen items, -judge -prior, same embedder seat
#   - record-only: this writes an archive under var/, never a threshold
#
# Usage:
#   scripts/semhead-ab.sh [LIMIT]        # default LIMIT=12 (paired McNemar is
#                                         # readable well before 30)
set -eu
cd "$(dirname "$0")/.."
N="${1:-12}"
BASE="$(pwd)/var/semhead-ab"
export CLUS_EMBED=minilm   # the L1 seat must match the corpus embedding

ask="$BASE/cumulus-cluster"
mkdir -p "$BASE"
go build -o "$ask" ./cmd/cumulus-cluster

# One frozen item set + corpus, built once, shared by both arms. Deterministic:
# realeval.sh prep draws the same first N triples from the same dataset.
if [ ! -f "$BASE/items.jsonl" ]; then
	STATE="$BASE" CNLAW_DIR="${CNLAW_DIR:-$HOME/datasets/cn-law-rag/finetune_dataset.jsonl}" \
		REALEVAL_N=30 scripts/realeval.sh prep
	mv "$BASE"/../realeval/items.jsonl "$BASE/items.jsonl" 2>/dev/null ||
		cp var/realeval/items.jsonl "$BASE/items.jsonl"
	cp var/realeval/corpus.jsonl "$BASE/corpus.jsonl"
	echo "ab: frozen items=$(wc -l < "$BASE/items.jsonl") docs=$(wc -l < "$BASE/corpus.jsonl")"
fi

run_arm() { # name semantic_head
	local name="$1" head="$2"
	local state="$BASE/$name"
	# Resumable, with a correction that cost a whole wasted arm the first time:
	# `eval-run -limit N` caps how many items THIS invocation processes, not how
	# many the output file may hold. A killed run leaves k rows, and re-running
	# with -limit N produced k+N — so head0 archived 19 rows against head4's 12
	# and the report-level aggregates stopped being comparable. The limit is
	# therefore the shortfall against the target, never the target.
	local have=0
	[ -s "$state/results.jsonl" ] && have="$(wc -l < "$state/results.jsonl" | tr -d ' ')"
	local want=$N
	if [ "$have" -ge "$want" ]; then
		echo "ab: $name — already complete ($have/$want item(s)), skipping"
		return 0
	fi
	local todo=$((want - have))
	# The store is only built on the first attempt, which is also the only time
	# the "fresh store per arm" discipline applies.
	if [ ! -d "$state/data" ]; then
		mkdir -p "$state"
		"$ask" -data "$state/data" ensure
		"$ask" -data "$state/data" ingest-jsonl -file "$BASE/corpus.jsonl" -job ab
		"$ask" -data "$state/data" ensure -embed >/dev/null
		echo "ab: $name — fresh store built"
	else
		echo "ab: $name — resuming from $have/$want, this pass takes $todo"
	fi
	echo "ab: $name — CLUS_ADMIT_SEMANTIC_HEAD=$head"
	CLUS_ADMIT_SEMANTIC_HEAD="$head" \
		"$ask" -data "$state/data" eval-run -file "$BASE/items.jsonl" \
		-out "$state/results.jsonl" -judge -prior -tag "semhead-$name" \
		-limit "$todo" > "$state/report.json"
	echo "ab: $name report written"
}

run_arm head0 0
run_arm head4 4

python3 scripts/semhead_ab_summary.py "$BASE"
