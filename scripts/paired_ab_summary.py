#!/usr/bin/env python3
"""Paired summary for scripts/paired-ab.sh. Read-only: prints, never writes.

Compares the two arms on the INTERSECTION of item ids only. `eval-run -limit` is
per-invocation, so an interrupted run can leave the arms at different lengths and
the two report.json aggregates are then not comparable — quoting them side by
side produced a wrong-looking-but-wrong reading once already.

Splits every metric by whether it passes the LLM judge, because that decides
whether a number can be trusted at all: `ev_rec` is a mechanical test (the gold
key is among the cited sources) while `correct` runs through the judge, which
scoreprobe measured flipping 10/30 verdicts on closed-book.
"""
import json
import os
import sys
from math import comb

# Arm names are configurable so this can also re-summarise an older two-arm
# directory whose arms were not named a0/a1 — used to cross-check that this
# script and the one it replaced read the same numbers.
A_ARM = os.environ.get("AB_ARM0", "a0")
B_ARM = os.environ.get("AB_ARM1", "a1")


ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def load(base, name):
    # Arms live under the repo (var/ab-*, var/semhead-ab, ...): keep the
    # reader inside it so a stray argument cannot aim at arbitrary files.
    if not os.path.realpath(base).startswith(ROOT + os.sep):
        raise SystemExit(f"ab base must live under the repo, got {base!r}")
    p = os.path.join(base, name, "results.jsonl")
    if not os.path.exists(p):
        return {}
    return {r["id"]: r for r in map(json.loads, open(p)) if r.get("id")}


def ev(row, key):
    return bool((row.get("eval") or {}).get(key))


def mcnemar(b, c):
    n = b + c
    if n == 0:
        return 1.0
    k = min(b, c)
    return min(1.0, sum(comb(n, i) for i in range(k + 1)) / 2 ** n * 2)


def main():
    base = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("AB_BASE", "var/ab")
    tag = os.environ.get("AB_TAG", os.path.basename(base))
    var = os.environ.get("AB_VAR", "?")
    vals = os.environ.get("AB_VALS", "a/b")

    A, B = load(base, A_ARM), load(base, B_ARM)
    if not A or not B:
        print(f"ab[{tag}]: need both arms ({A_ARM}={len(A)} {B_ARM}={len(B)})")
        return
    ids = sorted(set(A) & set(B))

    print(f"\n{'=' * 78}")
    print(f"paired A/B · {var}  {vals}   [{tag}]")
    print(f"archived rows: {A_ARM}={len(A)}  {B_ARM}={len(B)}  "
          f"→ report.json aggregates NOT comparable; paired n={len(ids)}")
    print("=" * 78)
    print(f'{"id":>7s} {"ev0":>6s} {"ev1":>6s} {"cor0":>6s} {"cor1":>6s} '
          f'{"call0":>6s} {"call1":>6s} {"tok0":>8s} {"tok1":>8s}')
    for i in ids:
        a, b = A[i], B[i]
        print(f'{i:>7s} {str(ev(a,"ev_rec")):>6s} {str(ev(b,"ev_rec")):>6s} '
              f'{str(ev(a,"correct")):>6s} {str(ev(b,"correct")):>6s} '
              f'{(a.get("calls") or 0):>6d} {(b.get("calls") or 0):>6d} '
              f'{(a.get("search_tokens") or 0):>8d} {(b.get("search_tokens") or 0):>8d}')

    print()
    print("metric   arm0  arm1   discordant 0-only/1-only   McNemar p   judge-free?")
    for m, judge_free in (("ev_rec", True), ("correct", False)):
        a = sum(1 for i in ids if ev(A[i], m))
        b = sum(1 for i in ids if ev(B[i], m))
        oa = sum(1 for i in ids if ev(A[i], m) and not ev(B[i], m))
        ob = sum(1 for i in ids if ev(B[i], m) and not ev(A[i], m))
        print(f"{m:>7s}  {a:>4d}  {b:>4d}   {oa:>10d} /{ob:<6d}   {mcnemar(oa, ob):>9.4f}   "
              f"{'yes' if judge_free else 'NO (judge flips 10/30)'}")

    print()
    for k in ("calls", "search_tokens", "loops"):
        a = sum((A[i].get(k) or 0) for i in ids)
        b = sum((B[i].get(k) or 0) for i in ids)
        if a:
            print(f"{k:>14s}: {A_ARM}={a}  {B_ARM}={b}  ({b / a:.2f}x)")


if __name__ == "__main__":
    main()
