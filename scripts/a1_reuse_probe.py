#!/usr/bin/env python3
"""A-1: measure the cluster reuse hit rate against a live serve.

What this measures
------------------
The product headline is "同类问题越问越快". The internal/monitor package
already tracks it (`reuse_hits`, `reuse_rate`, `warm_count`, `warm_p50_ms`,
`cold_count`) and exposes it read-only at GET /v1/monitor/overview. So this
script needs no production change: it builds a corpus, drives a serve with a
question pattern, and reads the counter back.

Why the question pattern matters
--------------------------------
The archived run that produced Ev.Rec 51.7% already implies a reuse hit rate
of 53/60 = 88.3% (53 FAST rows with zero LLM calls) — but that run re-asked
the SAME batch, which is the most favourable case. This script uses a
deliberately harder pattern:
  * 3 questions per topic, phrased differently (not identical repeats)
  * topics visited in bursts, like a real session
  * every topic revisited LATER in the run, so a "hit" needs the cluster to
    survive topic switches — this is the "越问越快" claim in its real form

Cost
----
Runs the suite in CLUS_OFFLINE mode, so the analyzer/synthesizer are the
deterministic stubs and no model is billed. The caveat is written into the
report: offline stubs may refuse, and a refusal never persists a cluster, so
this measures the reuse path's upper bound, not its production rate.

Usage
-----
  python3 scripts/a1_reuse_probe.py --port 18115 [--questions 24] [--keep]
"""

import argparse
import json
import os
import random
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CORPUS = os.path.join(REPO, "var/chinalaw/corpus.jsonl")
ITEMS = os.path.join(REPO, "var/golden/items_raw.jsonl")


def http(url, payload=None, timeout=60):
    data = None
    headers = {}
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def get(url, timeout=20):
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return json.loads(r.read().decode())


def load_rows(path):
    return [json.loads(l) for l in open(path) if l.strip()]


def build_corpus(work, n_questions, n_background, seed=7):
    """One file per document.

    Concatenating the corpus into a single .md was tried first and it is
    WRONG: the suite's ingest unit is the source document, so a 724-doc
    concatenation ingests as ONE source and the sampler reports sampled=0
    while BM25 over the same text still finds gold for 91.7% of the
    questions. Retrieval happens at source granularity — feed it sources.
    """
    corpus = {}
    for row in load_rows(CORPUS):
        corpus[row["key"]] = row
    items = [it for it in load_rows(ITEMS) if it.get("gold_sources") and it["gold_sources"][0] in corpus]
    if len(items) < n_questions:
        raise SystemExit(f"only {len(items)} usable items, need {n_questions}")
    sel = items[:n_questions]
    golds = {it["gold_sources"][0] for it in sel}
    rest = [k for k in corpus if k not in golds]
    random.seed(seed)
    keys = list(golds) + random.sample(rest, n_background)

    docs = os.path.join(work, "docs")
    os.makedirs(docs, exist_ok=True)
    for k in keys:
        safe = "".join(c if c.isalnum() or c in "-_" else "_" for c in k)[:60]
        with open(os.path.join(docs, safe + ".md"), "w") as f:
            f.write("# " + corpus[k]["title"] + "\n\n" + corpus[k]["text"] + "\n")
    return sel, keys


def paraphrase(q):
    """Two surface variants of one question.

    Not linguistic paraphrases — just the two common rewordings: a wh-form
    and a "what should I do about" form. The point is to defeat an EXACT
    string match on the cluster's query key, not to test semantics.
    """
    s = q.rstrip("？?。 ")
    if "怎么办" in s or "该怎么" in s:
        return [s + "？", "遇到这种情况要怎么处理？", "具体应该走什么流程？"]
    return [s + "？", s + "，具体是什么规定？", "关于这个问题，依据是哪一条？"]


def pattern(topics):
    """Bursts, then a revisit pass.

    Returns a list of (label, query) where label is the topic index so the
    script can tell a first visit from a revisit.
    """
    seq = []
    for i, q in enumerate(topics):
        for v in paraphrase(q):
            seq.append((i, v))
    for i, q in enumerate(topics):          # revisit every topic, later
        seq.append((i, paraphrase(q)[0]))
    return seq


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=18115)
    ap.add_argument("--questions", type=int, default=24)
    ap.add_argument("--background", type=int, default=700)
    ap.add_argument("--keep", action="store_true", help="keep the temp workdir")
    ap.add_argument("--out", default="", help="write the report JSON here")
    ap.add_argument("--live", action="store_true",
                    help="talk to the real endpoint (bills tokens). Offline stubs "
                         "refuse these questions, so no cluster is ever written and "
                         "reuse_hits stays 0 — that is why the measurement needs this.")
    ap.add_argument("--token-cap", type=int, default=0,
                    help="abort the drive once the run reports this many tokens")
    args = ap.parse_args()

    work = tempfile.mkdtemp(prefix="a1reuse.")
    store = os.path.join(work, "store")
    os.makedirs(store)
    binary = os.path.join(work, "cumulus-cluster")
    print(f"A-1 复用命中率复测   workdir={work}")

    print("  build binary …", flush=True)
    subprocess.run(["go", "build", "-o", binary, "./cmd/cumulus-cluster"], cwd=REPO, check=True)

    print(f"  build corpus ({args.questions} topics + {args.background} background docs) …", flush=True)
    items, keys = build_corpus(work, args.questions, args.background)
    topics = [it["query"] for it in items]

    if args.live:
        # No CLUS_OFFLINE, no CLUS_ENV=/dev/null — the suite must read the
        # operator's ./.env so the real analyzer/synthesizer are wired.
        env = dict(os.environ)
        env.pop("CLUS_OFFLINE", None)
    else:
        env = dict(os.environ, CLUS_OFFLINE="1", CLUS_ENV="/dev/null",
                   CLUS_MODEL_DIR=os.path.join(work, "nomodel"))
    print("  ingest …", flush=True)
    r = subprocess.run([binary, "-data", store, "ingest-files",
                        "-dir", os.path.join(work, "docs"), "-recursive", "-job", "j1"],
                       env=env, capture_output=True, text=True)
    print("   ", r.stdout.strip().replace("\n", " ")[:80] or r.stderr.strip()[:120])

    print("  serve …", flush=True)
    log = open(os.path.join(work, "serve.log"), "w")
    proc = subprocess.Popen([binary, "-data", store, "serve", "-listen", f"127.0.0.1:{args.port}"],
                            env=env, stdout=log, stderr=log)
    base = f"http://127.0.0.1:{args.port}"
    try:
        for _ in range(40):
            try:
                get(base + "/v1/monitor/overview", timeout=2)
                break
            except Exception:
                time.sleep(0.5)
        else:
            raise SystemExit("serve did not come up")
        http(base + "/v1/buckets", {"name": "law"})

        seq = pattern(topics)
        if args.token_cap:
            print(f"  token cap = {args.token_cap}（超限即停）\n", flush=True)
        print(f"  drive {len(seq)} queries ({len(topics)} topics x3 + {len(topics)} revisits) …\n", flush=True)
        rows = []
        for n, (topic, q) in enumerate(seq, 1):
            t0 = time.time()
            try:
                d = http(base + "/v1/search", {"query": q, "ns": "law"}, timeout=90)
            except Exception as e:
                print(f"    {n:2d}. ERROR {e}")
                continue
            a = d.get("answer", {})
            rows.append({
                "n": n, "topic": topic, "query": q,
                "mode": d.get("mode"), "reused": d.get("reused"),
                "sampled": d.get("sampled"), "loops": d.get("loops"),
                "llm_calls": a.get("llm_calls"), "refused": a.get("refused"),
                "conf": a.get("confidence"), "wall_ms": int((time.time() - t0) * 1000),
            })
            mark = "WARM" if d.get("reused") else "cold"
            print(f"    {n:2d}. {mark}  mode={d.get('mode'):4s} topic#{topic} "
                  f"conf={a.get('confidence')} refused={a.get('refused')}")
            if args.token_cap:
                used = get(base + "/v1/monitor/overview").get("llm", {}).get("tokens", 0)
                if used >= args.token_cap:
                    print(f"    !! token cap reached ({used} >= {args.token_cap}), 停止")
                    break

        snap = get(base + "/v1/monitor/overview")
        r_ = snap.get("retrieval", {})
        print("\n=== monitor 读数 ===")
        for k in ("queries",):
            print(f"  {k} = {snap.get(k)}")
        for k, v in (snap.get("llm") or {}).items():
            if k in ("calls", "tokens", "tokens_per_query", "calls_per_min"):
                print(f"  llm.{k} = {v}")
        for k in ("by_mode", "reuse_hits", "reuse_rate", "warm_count", "cold_count",
                  "warm_p50_ms", "escalations", "refused"):
            if k in r_:
                print(f"  {k} = {r_[k]}")

        first = [x for x in rows if x["topic"] >= 0 and x["n"] <= 3 * len(topics)]
        revisit = rows[3 * len(topics):]
        rep = {
            "topics": len(topics), "queries": len(rows),
            "monitor": r_, "rows": rows,
            "revisit_hits": sum(1 for x in revisit if x["reused"]),
            "revisit_n": len(revisit),
            "first_visit_hits": sum(1 for x in first if x["reused"]),
        }
        print(f"\n  首访命中 {rep['first_visit_hits']} / {len(first)}")
        print(f"  回访命中 {rep['revisit_hits']} / {len(revisit)}"
              f"  ⇒ 回访复用率 {100*rep['revisit_hits']/max(1,len(revisit)):.1f}%")
        refused = sum(1 for x in rows if x["refused"])
        print(f"  拒答 {refused} / {len(rows)}  （离线桩拒答不会成簇，会压低复用读数）")

        if args.out:
            with open(args.out, "w") as f:
                json.dump(rep, f, ensure_ascii=False, indent=2)
            print(f"\n  report → {args.out}")
    finally:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=10)
        except Exception:
            proc.kill()
        if args.keep:
            print(f"  kept {work}")
        else:
            shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
