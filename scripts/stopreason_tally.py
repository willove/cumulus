#!/usr/bin/env python3
"""Tally the stop_reason distribution from an eval results.jsonl.

Reads the JSONL on STDIN (the caller owns path handling):

    python3 scripts/stopreason_tally.py < var/stopreason-probe/arm/results.jsonl

Cross-tabs the DEEP-loop exits against mode, loops, tokens and the judge
verdict, so the histogram reads against correctness rather than as bare
counts. FAST cache hits and errors carry no stop_reason and show as (none).
"""
import collections
import json
import sys


def median(xs):
    xs = sorted(xs)
    if not xs:
        return 0
    return xs[len(xs) // 2] if len(xs) % 2 else (xs[len(xs) // 2 - 1] + xs[len(xs) // 2]) / 2


def main(stream):
    rows = [json.loads(line) for line in stream if line.strip()]
    stop = collections.Counter()
    by_stop = collections.defaultdict(lambda: {
        "n": 0, "ok": 0, "judged": 0, "loops": [], "search_toks": [],
        "modes": collections.Counter(),
    })
    for r in rows:
        mode = r.get("mode") or "?"
        sr = r.get("stop_reason") or ""
        if mode == "error":
            sr, mode = "(error)", "error"
        elif not sr:
            sr = "(none)"
        stop[sr] += 1
        d = by_stop[sr]
        d["n"] += 1
        d["modes"][mode] += 1
        d["loops"].append(r.get("loops") or 0)
        d["search_toks"].append(r.get("search_tokens") or r.get("tokens") or 0)
        ev = r.get("eval") or {}
        if ev.get("correct") is not None:
            d["judged"] += 1
            if ev.get("correct"):
                d["ok"] += 1

    total = sum(stop.values())
    print(f"== stop_reason distribution ({total} items) ==")
    for sr, n in stop.most_common():
        d = by_stop[sr]
        modes = ",".join(f"{m}x{c}" for m, c in d["modes"].most_common())
        acc = f"{d['ok']}/{d['judged']}" if d["judged"] else "-"
        print(f"{sr:26s} n={n:3d} ({n/total:5.1%})  modes[{modes}]  "
              f"correct={acc}  loops p50={median(d['loops']):.0f}  "
              f"search_tok p50={median(d['search_toks']):.0f}")

    deep = [r for r in rows if (r.get("mode") or "").upper().startswith("DEEP")]
    if deep:
        exited = [r for r in deep if r.get("stop_reason")]
        print(f"\nDEEP-mode items: {len(deep)}, with an explicit exit: {len(exited)}")


if __name__ == "__main__":
    main(sys.stdin)
