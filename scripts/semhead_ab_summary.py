#!/usr/bin/env python3
"""Paired summary for scripts/semhead-ab.sh.

Read-only: prints, never writes. Compares the two arms on the INTERSECTION of
item ids, because `eval-run -limit` is per-invocation and an interrupted run can
leave the arms at different lengths — report.json aggregates are then not
comparable and must not be quoted side by side.

Writes nothing, decides nothing. The numbers are a reading, not a gate.
"""
import json
import os
import sys
from math import comb

A_ARM, B_ARM = "head0", "head4"


def load(base, name):
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
    base = sys.argv[1] if len(sys.argv) > 1 else "var/semhead-ab"
    A, B = load(base, A_ARM), load(base, B_ARM)
    if not A or not B:
        print("ab: need both arms")
        return
    ids = sorted(set(A) & set(B))
    print(f"\n=== P1-3 paired A/B · CLUS_ADMIT_SEMANTIC_HEAD 0 vs 4 · paired n={len(ids)} ===")
    print(f"(archived rows: {A_ARM}={len(A)}  {B_ARM}={len(B)} — report-level "
          f"aggregates are NOT comparable; only the paired rows are)")
    print()
    print(f'{"id":>7s} {"ev0":>6s} {"ev4":>6s} {"cor0":>6s} {"cor4":>6s} '
          f'{"call0":>6s} {"call4":>6s} {"tok0":>8s} {"tok4":>8s}')
    for i in ids:
        a, b = A[i], B[i]
        print(f'{i:>7s} {str(ev(a,"ev_rec")):>6s} {str(ev(b,"ev_rec")):>6s} '
              f'{str(ev(a,"correct")):>6s} {str(ev(b,"correct")):>6s} '
              f'{(a.get("calls") or 0):>6d} {(b.get("calls") or 0):>6d} '
              f'{(a.get("search_tokens") or 0):>8d} {(b.get("search_tokens") or 0):>8d}')

    print()
    for m in ("ev_rec", "correct"):
        a = sum(1 for i in ids if ev(A[i], m))
        b = sum(1 for i in ids if ev(B[i], m))
        onlyA = sum(1 for i in ids if ev(A[i], m) and not ev(B[i], m))
        onlyB = sum(1 for i in ids if ev(B[i], m) and not ev(A[i], m))
        p = mcnemar(onlyA, onlyB)
        print(f"{m:>8s}: {A_ARM}={a}/{len(ids)}  {B_ARM}={b}/{len(ids)}  "
              f"discordant {A_ARM}-only={onlyA} {B_ARM}-only={onlyB}  p={p:.4f}")

    print()
    for k in ("calls", "search_tokens", "loops"):
        a = sum((A[i].get(k) or 0) for i in ids)
        b = sum((B[i].get(k) or 0) for i in ids)
        if a:
            print(f"{k:>14s}: {A_ARM}={a}  {B_ARM}={b}  ({b / a:.2f}x)")


if __name__ == "__main__":
    main()
