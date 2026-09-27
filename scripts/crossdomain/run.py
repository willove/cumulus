#!/usr/bin/env python3
"""Cross-domain harness runner: one frozen question set, one real pipeline
run, one archived regime profile.

The runner's discipline:
  1. VERIFY the set's hash against its manifest — a tampered or regenerated
     set refuses to run (the golden set's rule);
  2. run the real pipeline via eval-run (live model + Closed-Book control
     + judge + L1-pre narrowing), nothing mocked;
  3. archive the report, the per-item results and the exact config under
     results/crossdomain/<tag>/;
  4. print the regime profile — the cross-domain comparison is over
     BEHAVIOUR (mode mix, confidence shape, refusal rate, loops), not over
     absolute scores, which are not comparable across domains.

Usage:
  run.py --domain baike --tag baike-baseline [--limit N] [--env K=V ...]
"""
import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
BIN = os.path.join(ROOT, "bin", "cumulus-cluster")
RESULTS = os.path.join(ROOT, "results", "crossdomain")

# Store identity per domain. A domain is a bucket, not a code mode: new
# domains add a line here and nothing else.
DOMAINS = {
    "baike": {
        "data": "var/cumulus-baike", "ns": "baike",
        "sources": "clus_sources", "evidence": "clus_evidence",
    },
    "chinalaw": {
        "data": "var/cumulus-cluster", "ns": "chinalaw",
        "sources": "clus_sources", "evidence": "clus_evidence",
    },
}


def sha256(path):
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


def verify_manifest(domain, setpath=None, manpath=None):
    qpath = setpath or os.path.join(HERE, "questions", domain + ".jsonl")
    manpath = manpath or os.path.join(HERE, "questions", domain + ".manifest.sha256")
    if not os.path.exists(qpath) or not os.path.exists(manpath):
        raise SystemExit(f"{domain}: no frozen set (need {qpath} + manifest)")
    man = json.load(open(manpath, encoding="utf-8"))
    key = os.path.basename(qpath)
    want = man["sha256"].get(key)
    if want is None:  # golden manifests key by repo-relative path
        for k, v in man["sha256"].items():
            if k.endswith(key):
                want = v
                break
    if want is None:
        raise SystemExit(f"{manpath} does not cover {key}")
    got = sha256(qpath)
    if got != want:
        raise SystemExit(f"{domain}: question set fails its manifest ({got[:12]} != {want[:12]}) — frozen sets are immutable")
    return qpath, man


def profile(results_path, report):
    """Regime profile from the per-item results + the aggregate report."""
    rows = [json.loads(l) for l in open(results_path, encoding="utf-8") if l.strip()]
    n = len(rows)
    modes, confs, loops, skipped = {}, [], 0, 0
    for r in rows:
        modes[r.get("mode", "?")] = modes.get(r.get("mode", "?"), 0) + 1
        if r.get("conf") is not None:
            confs.append(r["conf"])
        loops += r.get("loops") or 0
        if r.get("skipped"):
            skipped += 1
    confs.sort()
    q = lambda p: (confs[int(p * (len(confs) - 1))] if confs else None)
    sysm = report.get("system", {})
    tx = sysm.get("taxonomy", {})
    cb = report.get("closed_book", {})
    print("\n=== regime profile ===")
    print(f"items            {n}   (skipped/refused-by-system {skipped})")
    print(f"mode mix         {modes}")
    if confs:
        print(f"confidence       p10={q(0.1):.2f} p50={q(0.5):.2f} p90={q(0.9):.2f} mean={sum(confs)/len(confs):.2f}")
    print(f"loops            mean {loops / max(n, 1):.1f}/item")
    print(f"taxonomy         {tx}")
    print(f"ev_rec           system={sysm.get('ev_rec')} closed_book={cb.get('ev_rec')}")
    print(f"em               system={sysm.get('em')} closed_book={cb.get('em')}")
    print(f"ground           system={sysm.get('ground')} closed_book={cb.get('ground')}")
    print(f"mcnemar          {report.get('mcnemar')}")
    print(f"tokens           search={report.get('search_tokens')} judge={report.get('judge_tokens')}")
    nr = report.get("nr_breakdown") or report.get("rejected_proposals")
    if nr is not None:
        print(f"nr/proposals     {nr}")
    return {"n": n, "modes": modes, "conf_p50": q(0.5), "skipped": skipped}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--domain", required=True, choices=sorted(DOMAINS))
    ap.add_argument("--tag", required=True)
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--set", help="frozen items jsonl (default: questions/<domain>.jsonl; the chinalaw golden sets under testdata/eval/ work here)")
    ap.add_argument("--manifest", help="manifest covering --set (default: questions/<domain>.manifest.sha256)")
    ap.add_argument("--env", action="append", default=[], help="K=V passed to the run")
    ap.add_argument("--reset", action="store_true",
                    help="clear derived learning state (clusters/cites/evidence/sessions) before the run, so every arm starts at the same temperature — without it arm 2 reuses arm 1's clusters (the mode-mix drift this prevents is visible in baike-baseline vs baike-baseline2: FAST 9 → 20)")
    args = ap.parse_args()

    qpath, man = verify_manifest(args.domain, args.set, args.manifest)
    d = DOMAINS[args.domain]
    outdir = os.path.join(RESULTS, args.tag)
    os.makedirs(outdir, exist_ok=True)
    results = os.path.join(outdir, "results.jsonl")

    env = dict(os.environ)
    for kv in args.env:
        k, _, v = kv.partition("=")
        env[k] = v
    # The L1 seat must be the one the corpus was embedded with — otherwise
    # the comparison measures embedder mismatch, not domain behaviour.
    env.setdefault("CLUS_EMBED", "minilm")

    cmd = [BIN, "-data", d["data"], "-ns", d["ns"],
           "-sources", d["sources"], "-evidence", d["evidence"],
           "eval-run", "-file", qpath, "-out", results,
           "-judge", "-prior", "-l1pre", "-tag", args.tag]
    if args.limit:
        cmd += ["-limit", str(args.limit)]
    if args.reset:
        rst = [BIN, "-data", d["data"], "-ns", d["ns"], "-evidence", d["evidence"],
               "reset", "learned", "-yes"]
        print("+ " + " ".join(rst))
        r = subprocess.run(rst, cwd=ROOT, env=env, capture_output=True, text=True)
        if r.returncode != 0:
            raise SystemExit(f"reset failed: {r.stderr[:300]}")
    print("+ " + " ".join(cmd))
    p = subprocess.run(cmd, cwd=ROOT, env=env, capture_output=True, text=True)
    sys.stderr.write(p.stderr[-2000:])
    if p.returncode != 0:
        raise SystemExit(f"eval-run failed: rc={p.returncode}")
    try:
        report = json.loads(p.stdout)
    except json.JSONDecodeError:
        raise SystemExit("eval-run stdout was not JSON:\n" + p.stdout[:400])
    json.dump(report, open(os.path.join(outdir, "report.json"), "w", encoding="utf-8"),
              ensure_ascii=False, indent=2)
    json.dump({"domain": args.domain, "tag": args.tag, "manifest": man,
               "env": {k: env[k] for k in sorted(env) if k.startswith(("CLUS_", "AIGATE_")) and "KEY" not in k},
               "argv": cmd},
              open(os.path.join(outdir, "config.json"), "w", encoding="utf-8"),
              ensure_ascii=False, indent=2)
    profile(results, report)
    print(f"\narchived: {outdir}")


if __name__ == "__main__":
    main()
