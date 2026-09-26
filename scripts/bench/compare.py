#!/usr/bin/env python3
"""bench compare: 对比两次 run 的指标（control vs experiment）。

用法: python3 scripts/bench/compare.py off on [--tag-group gap,follow,cold]

输出每场景：总墙钟、p50/p95、insufficient 数、与首问引用交集（知识是否被
后续追问复用）、总 token。Δ 列为 experiment − control（负数=变好）。
"""
import argparse
import json
import os
import statistics as st

BENCH = os.path.dirname(os.path.abspath(__file__))


def load(tag):
    p = os.path.join(BENCH, f"results-{tag}.json")
    if not os.path.exists(p):
        raise SystemExit(f"missing {p} — run it first")
    return json.load(open(p))


def agg(run):
    walls = [r["wall_ms"] for r in run["rows"]]
    total = sum(walls)
    p50 = st.median(walls) if walls else 0
    p95 = sorted(walls)[max(0, int(len(walls) * 0.95) - 1)] if walls else 0
    ins = sum(1 for r in run["rows"] if r["insufficient"])
    # 与首问引用交集：后续追问是否复用了首问的文档（知识关联的直接证据）
    first = set(run["rows"][0]["docs"]) if run["rows"] else set()
    overlap = len(first.intersection(*[set(r["docs"]) for r in run["rows"][1:]])) if len(run["rows"]) > 1 else 0
    toks = sum(r.get("tokens", 0) for r in run["rows"])
    return {"total": total, "p50": int(p50), "p95": int(p95), "ins": ins, "overlap": overlap, "toks": toks}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("control")
    ap.add_argument("experiment")
    ap.add_argument("--tag-group", default="")
    args = ap.parse_args()
    a, b = load(args.control), load(args.experiment)
    groups = set(args.tag_group.split(",")) if args.tag_group else None

    print(f"\n{'scenario':<18} {'arm':<4} {'total':>8} {'p50':>7} {'p95':>7} {'ins':>4} {'∩首问':>5} {'tokens':>8}")
    print("-" * 74)
    sums = {a["tag"]: {"total": 0, "ins": 0}, b["tag"]: {"total": 0, "ins": 0}}
    for ra, rb in zip(a["runs"], b["runs"]):
        if groups and ra.get("tag") not in groups:
            continue
        ga, gb = agg(ra), agg(rb)
        for r, g, tag in ((ra, ga, a["tag"]), (rb, gb, b["tag"])):
            print(f"{r['name']:<18} {tag:<4} "
                  f"{g['total']:>7}ms {g['p50']:>6}ms {g['p95']:>6}ms {g['ins']:>4} {g['overlap']:>5} {g['toks']:>8}")
            sums[tag]["total"] += g["total"]
            sums[tag]["ins"] += g["ins"]
        d = gb["total"] - ga["total"]
        print(f"{'':<18} Δ    {d:>+7}ms ({'+' if d >= 0 else ''}{d / max(ga['total'], 1) * 100:.0f}%)\n")

    print(f"合计: {a['tag']}={sums[a['tag']]['total']}ms  {b['tag']}={sums[b['tag']]['total']}ms  "
          f"Δ={sums[b['tag']]['total'] - sums[a['tag']]['total']:+}ms")


if __name__ == "__main__":
    main()
