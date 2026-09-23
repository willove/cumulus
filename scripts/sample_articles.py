#!/usr/bin/env python3
"""Deterministic article sampling for the θ probe.

Picks N articles by stride from the full statute corpus and writes:
  corpus.jsonl — {key,title,text} rows (also the anchorgen input)
No LLM, no golden set.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path


def main() -> None:
    src, out, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
    rows = [json.loads(l) for l in Path(src).read_text(encoding="utf-8").splitlines() if l.strip()]
    step = max(1, len(rows) // max(1, n))
    picked = rows[::step][:n]
    with Path(out).open("w", encoding="utf-8") as f:
        for r in picked:
            f.write(json.dumps(r, ensure_ascii=False) + "\n")
    print(f"articles={len(picked)}", file=sys.stderr)


if __name__ == "__main__":
    main()
