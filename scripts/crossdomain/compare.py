#!/usr/bin/env python3
"""Cross-domain matrix: put the per-domain regime profiles side by side.

The comparison is over BEHAVIOUR, not absolute scores: a village-encyclopedia
domain and a statute domain have incomparable question difficulty, so EM
percentages are reported per domain and NOT differenced. What the matrix
answers: does the machine behave the same way — same mode mix, same
confidence shape, same refusal discipline, same admission behaviour — when
only the corpus changes? A regime that shifts sharply between domains is
either a coupling the audit missed or a constant that only holds for the
domain it was tuned on.

Usage:
  compare.py baike-baseline chinalaw-baseline [more tags...]
"""
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
RESULTS = os.path.join(ROOT, "results", "crossdomain")


def rows(tag):
    p = os.path.join(RESULTS, tag, "results.jsonl")
    return [json.loads(l) for l in open(p, encoding="utf-8") if l.strip()]


def conf_q(rows, p):
    cs = sorted(r["conf"] for r in rows if r.get("conf") is not None)
    return cs[int(p * (len(cs) - 1))] if cs else None


def f2(v):
    return "%.2f" % v if v is not None else "-"


def load(tag):
    rep = json.load(open(os.path.join(RESULTS, tag, "report.json"), encoding="utf-8"))
    rs = rows(tag)
    modes = {}
    for r in rs:
        modes[r.get("mode", "?")] = modes.get(r.get("mode", "?"), 0) + 1
    n = len(rs)
    sysm, cb = rep.get("system", {}), rep.get("closed_book", {})
    return {
        "tag": tag, "domain": rep.get("domain") or rep.get("ns") or "?",
        "n": n,
        "modes": modes,
        "conf_p50": conf_q(rs, 0.5),
        "conf_p10": conf_q(rs, 0.1),
        "loops": sum(r.get("loops") or 0 for r in rs) / max(n, 1),
        "skipped": sum(1 for r in rs if r.get("skipped")),
        "err": sum(1 for r in rs if r.get("mode") == "error"),
        "taxonomy": sysm.get("taxonomy", {}),
        "ev_rec": (sysm.get("ev_rec"), cb.get("ev_rec")),
        "em": (sysm.get("em"), cb.get("em")),
        "ground": (sysm.get("ground"), cb.get("ground")),
        "gold_in_corpus": rep.get("gold_in_corpus"),
        "gold_missing": rep.get("gold_missing_from_corpus"),
        "mcnemar": rep.get("mcnemar", {}),
        "tokens": (rep.get("search_tokens"), rep.get("judge_tokens")),
    }


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    tags = sys.argv[1:]
    profs = [load(t) for t in tags]
    w = max(len(p["tag"]) for p in profs) + 2
    print("\n=== cross-domain regime matrix ===")
    header = " | ".join("%16s" % p["tag"] for p in profs)
    print(f"{'':<{w}}{header}")
    for label, fn in [
        ("items", lambda p: str(p["n"])),
        ("modes", lambda p: "/".join(f"{k[:4]}:{v}" for k, v in sorted(p["modes"].items()))),
        ("conf p10", lambda p: f2(p["conf_p10"])),
        ("conf p50", lambda p: f2(p["conf_p50"])),
        ("loops/item", lambda p: f"{p['loops']:.1f}"),
        ("skipped", lambda p: str(p["skipped"])),
        ("errors", lambda p: str(p["err"])),
        ("ev_rec sys/cb", lambda p: f"{p['ev_rec'][0]}/{p['ev_rec'][1]}"),
        ("em sys/cb", lambda p: f"{p['em'][0]}/{p['em'][1]}"),
        ("ground sys/cb", lambda p: f"{p['ground'][0]}/{p['ground'][1]}"),
        ("gold in corpus", lambda p: str(p["gold_in_corpus"])),
        ("gold missing", lambda p: str(p["gold_missing"])),
        ("tokens s/j", lambda p: f"{p['tokens'][0]}/{p['tokens'][1]}"),
    ]:
        print(f"{label:<{w}}" + " | ".join(f"{fn(p):>16}" for p in profs))
    print("\nnote: absolute EM/ev_rec are NOT differenced across domains —")
    print("question difficulty is domain-specific by construction. Compare the")
    print("SHAPE: mode mix, confidence distribution, refusal/loop discipline.")


if __name__ == "__main__":
    main()
