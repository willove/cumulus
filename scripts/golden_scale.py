#!/usr/bin/env python3
"""Scale the golden eval set deterministically (R-E2 follow-up).

chinalaw39 was stride-sampled from the 14313-article statute corpus and then
rewritten into colloquial questions by `anchorgen` (LLM). This script picks
MORE articles with a different stride, EXCLUDING the ids already in the
existing golden set, and emits the corpus rows anchorgen consumes.

The sample is deterministic (no cherry-picking): selection cannot favor the
system, which is the whole point of a frozen golden set.

Usage:
  python3 scripts/golden_scale.py --existing testdata/eval/chinalaw39.jsonl \
      --n 120 > var/golden/corpus.jsonl
Then:
  ANCHOR_KEY=... go run ./cmd/anchorgen -n 120 < var/golden/corpus.jsonl \
      > testdata/eval/chinalawN.jsonl
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default="var/chinalaw/corpus.jsonl",
                    help="the full article corpus (key/title/text)")
    ap.add_argument("--existing", default="testdata/eval/chinalaw39.jsonl",
                    help="golden set whose ids must not be resampled")
    ap.add_argument("--n", type=int, default=120)
    ap.add_argument("--phase", type=int, default=60,
                    help="stride phase offset (avoids aligned overlap)")
    args = ap.parse_args()

    rows = [json.loads(l) for l in Path(args.corpus).read_text(encoding="utf-8").splitlines() if l.strip()]
    taken = set()
    if Path(args.existing).exists():
        for line in Path(args.existing).read_text(encoding="utf-8").splitlines():
            if line.strip():
                taken.update(json.loads(line).get("gold_sources") or [])
    stride = max(1, len(rows) // max(1, args.n))
    picked = []
    for i in range(args.phase % stride, len(rows), stride):
        r = rows[i]
        if r["key"] in taken:
            continue
        picked.append(r)
        if len(picked) >= args.n:
            break
    if len(picked) < args.n:
        print(f"warn: only {len(picked)} of {args.n} available after exclusions", file=sys.stderr)
    for r in picked:
        print(json.dumps(r, ensure_ascii=False))


if __name__ == "__main__":
    main()
