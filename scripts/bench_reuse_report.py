#!/usr/bin/env python3
"""Recompute the cluster-reuse reading from the archived bench runs.

Why this exists
---------------
optimization-plan §2.3 reports a reuse rate and a warm/cold cost gap derived
from scripts/bench/results-*.json. Those numbers were first produced by a
one-off shell command, which is not reviewable. This script makes the claim
reproducible AND — more importantly — checks whether it survives the obvious
confound.

The confound
------------
A naive "pool every row" comparison is not safe here. The archives are a
sequence of experiments against a code base that kept changing, so reused
rows cluster in the LATER runs. A warm row is cheap partly because it reused
and partly because that run also had a faster pipeline. Reporting the pooled
30x as "the cost of reuse" would credit the pipeline improvements to reuse.

So this reports three numbers, in increasing order of trust:

  pooled     every archived row, warm vs cold. Directional only.
  within-run rows compared only inside one (file, scenario) pair, so the run's
             code version, corpus and configuration are held fixed. This is
             the number to quote.
  dose       within-run, bucketed by reuse fraction of the scenario, to show
             the gradient rather than a single contrast.

Also flags the archived runs that are NOT clean baselines: scripts/bench/run.py
--reset is supposed to start from nothing, but several archives record
start_learning.clean = False with hundreds of learned documents, meaning the
reset did not take. Those are reported separately and excluded from the
headline.

Usage
-----
  python3 scripts/bench_reuse_report.py [--dir scripts/bench] [--json out.json]
"""

import argparse
import collections
import glob
import json
import os
import statistics as st
import sys


def load_runs(bench_dir):
    """Return [(path, tag, ns, reset, payload)] for every parseable archive."""
    out = []
    for path in sorted(glob.glob(os.path.join(bench_dir, "results-*.json"))):
        try:
            with open(path) as f:
                d = json.load(f)
        except Exception as e:
            print(f"  skip {os.path.basename(path)}: {e}", file=sys.stderr)
            continue
        out.append((path, os.path.basename(path)[len("results-"):-len(".json")],
                    d.get("ns"), d.get("reset"), d))
    return out


def rows_of(payload):
    for run in payload.get("runs", []):
        for r in run.get("rows", []):
            yield run.get("name"), r


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", default=os.path.join(
        os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "scripts", "bench"))
    ap.add_argument("--json", default="", help="write the full report here")
    args = ap.parse_args()

    runs = load_runs(args.dir)
    if not runs:
        raise SystemExit(f"no results-*.json under {args.dir}")
    print(f"归档：{len(runs)} 次运行，目录 {args.dir}\n")

    # ---- hygiene: which archives claim a reset but did not get one -------
    dirty = []
    for path, tag, ns, reset, d in runs:
        if not reset:
            continue
        start = d.get("start_learning") or {}
        if start.get("clean") is False:
            dirty.append((tag, start.get("total")))
    if dirty:
        print("⚠️ 以下归档带 --reset 但 start_learning.clean = False（reset 未生效，"
              "不能当干净基线，已从下方统计中排除）：")
        for tag, total in dirty:
            print(f"     {tag:<16} start_learning.total = {total}")
        print()

    usable = [(p, t, ns, r, d) for p, t, ns, r, d in runs if not (r and (d.get("start_learning") or {}).get("clean") is False)]
    print(f"参与统计的运行：{len(usable)} / {len(runs)}\n")

    # ---- 1. pooled ------------------------------------------------------
    cold, warm = [], []
    for _, _, _, _, d in usable:
        for _, r in rows_of(d):
            row = (r.get("tokens") or 0, r.get("wall_ms") or 0)
            (warm if r.get("reused") else cold).append(row)
    print("=== 1. 汇总（方向性，跨运行；受代码版本差异污染）===")
    if cold and warm:
        print(f"  未复用 n={len(cold):<4} token 中位 {st.median(t[0] for t in cold):>9.0f}"
              f"   延迟中位 {st.median(t[1] for t in cold)/1000:>6.1f}s")
        print(f"  已复用 n={len(warm):<4} token 中位 {st.median(t[0] for t in warm):>9.0f}"
              f"   延迟中位 {st.median(t[1] for t in warm)/1000:>6.1f}s")
        print(f"  复用率 {len(warm)}/{len(cold)+len(warm)} = {100*len(warm)/(len(cold)+len(warm)):.1f}%")
    print()

    # ---- 2. within-run: same (file, scenario) holds everything else fixed
    cells = collections.defaultdict(lambda: {"warm": [], "cold": []})
    for path, tag, _, _, d in usable:
        for name, r in rows_of(d):
            key = (tag, name)
            bucket = "warm" if r.get("reused") else "cold"
            cells[key][bucket].append((r.get("tokens") or 0, r.get("wall_ms") or 0))
    both = {k: v for k, v in cells.items() if v["warm"] and v["cold"]}
    print("=== 2. 同 run 同场景内对照（版本/语料/配置固定）—— 应以此为准 ===")
    if not both:
        print("  没有任何一个 (run, 场景) 同时含 warm 与 cold 行，无法做受控对照")
    else:
        rt, wl = [], []
        for (tag, name), v in sorted(both.items()):
            ct, cw = st.median(t[0] for t in v["cold"]), st.median(t[0] for t in v["warm"])
            lt, lw = st.median(t[1] for t in v["cold"]), st.median(t[1] for t in v["warm"])
            rt.append((tag, name, len(v["warm"]), len(v["cold"]), ct, cw, lt, lw))
        print(f"  {'run':<14}{'场景':<16}{'warm':>6}{'cold':>6}{'token 冷':>11}{'token 温':>11}{'降':>8}{'延迟降':>9}")
        for tag, name, nw, nc, ct, cw, lt, lw in rt:
            tr = ct / cw if cw else float("inf")
            lr = lt / lw if lw else float("inf")
            print(f"  {tag:<14}{str(name):<16}{nw:>6}{nc:>6}{ct:>11.0f}{cw:>11.0f}"
                  f"{tr:>7.1f}×{lr:>8.1f}×")
        good_tr = [r[4] / r[5] for r in rt if r[5]]
        good_lr = [r[6] / r[7] for r in rt if r[7]]
        if good_tr:
            print(f"\n  受控 token 降幅：最小 {min(good_tr):.1f}×  中位 {st.median(good_tr):.1f}×  最大 {max(good_tr):.1f}×")
            print(f"  受控延迟降幅：最小 {min(good_lr):.1f}×  中位 {st.median(good_lr):.1f}×  最大 {max(good_lr):.1f}×")
    print()

    # ---- 3. dose-response within run+scenario ----------------------------
    print("=== 3. 剂量梯度（同 run 同场景内，按复用比例分档）===")
    dose = collections.defaultdict(lambda: {"tok": [], "wall": [], "n": 0})
    for (tag, name), v in cells.items():
        tot = len(v["warm"]) + len(v["cold"])
        if not tot:
            continue
        frac = round(len(v["warm"]) / tot, 2)
        for row in v["warm"] + v["cold"]:
            dose[frac]["tok"].append(row[0])
            dose[frac]["wall"].append(row[1])
            dose[frac]["n"] += 1
    print(f"  {'复用比例':<10}{'n':>5}{'token 中位':>12}{'延迟中位':>11}")
    for frac in sorted(dose):
        d = dose[frac]
        print(f"  {frac:<10.2f}{d['n']:>5}{st.median(d['tok']):>12.0f}"
              f"{st.median(d['wall'])/1000:>9.1f}s")
    print()

    if args.json:
        with open(args.json, "w") as f:
            json.dump({
                "runs_total": len(runs), "runs_used": len(usable),
                "dirty_reset_runs": [{"tag": t, "start_total": n} for t, n in dirty],
                "within_run_cells": [
                    {"tag": t, "scenario": n, "warm": w, "cold": c,
                     "token_cold": ct, "token_warm": cw, "ms_cold": lt, "ms_warm": lw}
                    for t, n, w, c, ct, cw, lt, lw in rt],
                "dose": {str(k): {"n": v["n"],
                                  "token_median": st.median(v["tok"]),
                                  "ms_median": st.median(v["wall"])}
                         for k, v in dose.items()},
            }, f, ensure_ascii=False, indent=2)
        print(f"report → {args.json}")


if __name__ == "__main__":
    main()
