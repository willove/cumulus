#!/usr/bin/env python3
"""Cross-domain question-set generator (frozen once, hash-sealed).

The set is the harness's ground truth: generate ONCE against a frozen
corpus, seal it, and never tune it afterwards — the discipline the golden
set (testdata/eval) already lives by. Regenerating requires an explicit
--force and produces a visibly different manifest, so "we picked better
questions" can never masquerade as improvement.

Per stratum, the generator samples entries (seeded), asks the LLM for one
natural factual question plus a gold fact that MUST be a verbatim
substring of the entry's content (EM-scorable, no paraphrase drift), and
pins gold_sources to the entry's business key (the corpus identity the
eval face matches against).

Usage:
  gen_questions.py --corpus /Users/willove/datasets/baidu_baike --out baike
"""
import argparse
import hashlib
import json
import os
import random
import re
import sys
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
QDIR = os.path.join(HERE, "questions")

# Stratified sampling: 5 content strata + an explicit long-document stratum
# (the >20k-char entries — the 2000-rune embed truncation makes them a
# regime question of their own). Seed is recorded in the manifest.
STRATA = [
    ("village", 12, lambda t, c: bool(re.search(r"(村|寨|屯|堡|庄|社区)$", t)) or "隶属于" in c[:80]),
    ("species", 12, lambda t, c: any(k in c for k in ("体长", "昆虫", "栖息", "翅展", "属昆虫"))),
    ("book",    12, lambda t, c: "出版社" in c or ("作者" in c and "出版" in c)),
    ("media",   12, lambda t, c: any(k in c for k in ("小说", "电视剧", "导演", "主演"))),
    ("general", 12, lambda t, c: True),  # catch-all, sampled last
    ("longdoc",  4, lambda t, c: len(c) > 20000),
]
SEED = 20260927

SYS_BAIKE = (
    "你是评测题生成器。给定一个百科条目(标题+正文)，生成一个只能依据该条目回答的"
    "事实型简答题，并给出标准答案。硬性要求："
    "1) fact 必须是正文中逐字出现的连续片段，不超过 30 字；"
    "2) question 不得包含 fact 的答案本身；"
    "3) question 用自然中文名词性问句，问实体属性(归属/作者/类别/数值/定义)；"
    "4) 只输出 JSON：{\"question\": \"...\", \"fact\": \"...\"}"
)
SYS_LAW = (
    "你是法条评测题生成器。给定一部法律的正文，生成一个只能依据该法条回答的"
    "事实型简答题，并给出标准答案。硬性要求："
    "1) fact 必须是正文中逐字出现的连续片段，不超过 40 字(可含条号)；"
    "2) question 不得包含 fact 的答案本身；"
    "3) question 用自然口语化中文问一个具体规则(期限/金额/主体/程序/罚则)；"
    "4) 只输出 JSON：{\"question\": \"...\", \"fact\": \"...\"}"
)


def load_env(path):
    env = {}
    if not os.path.exists(path):
        return env
    for line in open(path, encoding="utf-8"):
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        env[k.strip()] = v.strip().strip('"').strip("'")
    return env


def chat(env, sys_msg, user_msg, retries=2):
    base = os.environ.get("AIGATE_BASE_URL") or env.get("LLM_BASE_URL") or ""
    key = os.environ.get("AIGATE_API_KEY") or env.get("LLM_API_KEY") or ""
    # Same alias table the CLI applies (LLM_* -> AIGATE_*, env wins).
    model = (os.environ.get("AIGATE_CHAT_MODEL") or env.get("LLM_CHAT_MODEL")
             or env.get("LLM_MODEL_NAME") or "")
    if not base or not key or not model:
        raise SystemExit("generator: endpoint not configured (need AIGATE_*/LLM_* base/key/model)")
    url = base.rstrip("/") + "/chat/completions"
    body = json.dumps({
        "model": model,
        "messages": [
            {"role": "system", "content": sys_msg},
            {"role": "user", "content": user_msg},
        ],
        "temperature": 0.2,
    }).encode()
    last = None
    for _ in range(retries + 1):
        req = urllib.request.Request(url, data=body, headers={
            "Content-Type": "application/json", "Authorization": "Bearer " + key,
        })
        try:
            with urllib.request.urlopen(req, timeout=120) as r:
                out = json.load(r)
            return out["choices"][0]["message"]["content"]
        except Exception as e:  # noqa: BLE001 - generator is a dev tool
            last = e
    raise RuntimeError(f"chat failed after {retries + 1} tries: {last}")


def extract_json(text):
    """Pull the answer object out of a reasoning model's reply.

    MiniMax-M3 thinks before answering, and its reasoning contains JSON
    DRAFTS of the very object we want — a greedy first-to-last brace match
    spans draft + prose + real answer and always fails (56/64 items were
    dropped that way before this existed). Order of preference: the fenced
    block (the model repeats its final answer there), then bare objects
    from LAST to FIRST (skipping the reasoning's drafts)."""
    def ok(o):
        return isinstance(o, dict) and o.get("question") and o.get("fact")

    for cand in reversed(re.findall(r"```(?:json)?\s*(\{.*?\})\s*```", text, re.S)):
        try:
            o = json.loads(cand)
            if ok(o):
                return o
        except json.JSONDecodeError:
            continue
    for m in reversed(re.findall(r"\{[^{}]*\}", text, re.S)):
        try:
            o = json.loads(m)
            if ok(o):
                return o
        except json.JSONDecodeError:
            continue
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", required=True, help="dataset dir holding test.json/validation.json")
    ap.add_argument("--out", required=True, help="domain name, e.g. baike")
    ap.add_argument("--force", action="store_true", help="overwrite an existing frozen set")
    ap.add_argument("--strata", help="override the sampling table, e.g. general:40,longdoc:4")
    ap.add_argument("--kind", default="baike", choices=["baike", "law"],
                    help="corpus kind: frames the question prompt (default baike)")
    args = ap.parse_args()

    qpath = os.path.join(QDIR, args.out + ".jsonl")
    provpath = os.path.join(QDIR, args.out + ".prov.jsonl")
    manpath = os.path.join(QDIR, args.out + ".manifest.sha256")
    if os.path.exists(qpath) and not args.force:
        raise SystemExit(f"{qpath} exists — frozen. --force only to regenerate (new manifest).")
    os.makedirs(QDIR, exist_ok=True)

    files = sorted(f for f in os.listdir(args.corpus) if f.endswith(".jsonl"))
    if not files:
        raise SystemExit(f"{args.corpus}: no *.jsonl corpus files")
    entries = []
    for fn in files:
        for line in open(os.path.join(args.corpus, fn), encoding="utf-8"):
            if line.strip():
                entries.append(json.loads(line))
    print(f"corpus: {len(entries)} entries from {files}")

    env = load_env(os.path.join(os.path.dirname(HERE), "..", ".env"))
    strata = STRATA
    if args.strata:
        byname = {n: (n, q, p) for n, q, p in STRATA}
        strata = []
        for part in args.strata.split(","):
            n, _, q = part.partition(":")
            if n not in byname:
                raise SystemExit(f"unknown stratum {n!r} (have: {sorted(byname)})")
            strata.append((byname[n][0], int(q), byname[n][2]))
    sysmsg = SYS_LAW if args.kind == "law" else SYS_BAIKE
    rng = random.Random(SEED)
    picked, seen = [], set()
    for name, quota, pred in strata:
        pool = [e for e in entries if e["uniqueKey"] not in seen and pred(e.get("title", ""), e.get("content", ""))]
        rng.shuffle(pool)
        take = pool[:quota]
        for e in take:
            seen.add(e["uniqueKey"])
            picked.append((name, e))
        print(f"stratum {name}: sampled {len(take)} (pool {len(pool)})")

    items, prov, failed = [], [], 0
    for i, (stratum, e) in enumerate(picked, 1):
        title, content, key = e.get("title", ""), e.get("content", ""), e["uniqueKey"]
        user = f"标题：{title}\n正文：{content[:3000]}"
        try:
            raw = chat(env, sysmsg, user)
        except Exception as ex:  # noqa: BLE001
            print(f"  [{i}] chat error: {ex}")
            failed += 1
            continue
        obj = extract_json(raw) if raw else None
        if not obj:
            failed += 1
            continue
        q = (obj.get("question") or "").strip()
        fact = (obj.get("fact") or "").strip()
        if not q or not fact or fact not in content:
            print(f"  [{i}] rejected (fact not verbatim in content): {fact[:30]!r}")
            failed += 1
            continue
        iid = f"{args.out}-{i:03d}"
        items.append({"id": iid, "query": q, "answer": fact, "gold_sources": [key]})
        prov.append({"id": iid, "stratum": stratum, "title": title,
                     "uniqueKey": key, "url": e.get("url", "")})
    print(f"generated {len(items)} items, {failed} dropped")

    with open(qpath, "w", encoding="utf-8") as f:
        for it in items:
            f.write(json.dumps(it, ensure_ascii=False) + "\n")
    with open(provpath, "w", encoding="utf-8") as f:
        for p in prov:
            f.write(json.dumps(p, ensure_ascii=False) + "\n")
    h_items = hashlib.sha256(open(qpath, "rb").read()).hexdigest()
    h_prov = hashlib.sha256(open(provpath, "rb").read()).hexdigest()
    with open(manpath, "w", encoding="utf-8") as f:
        f.write(json.dumps({
            "domain": args.out, "seed": SEED, "items": len(items),
            "corpus": os.path.abspath(args.corpus), "kind": args.kind,
            "quota": {n: q for n, q, _ in strata},
            "sha256": {args.out + ".jsonl": h_items, args.out + ".prov.jsonl": h_prov},
        }, ensure_ascii=False, indent=2) + "\n")
    print(f"sealed: {manpath}")
    print(f"  items sha256 {h_items[:16]}…")


if __name__ == "__main__":
    main()
