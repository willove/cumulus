#!/usr/bin/env python3
"""prep_dureader.py —— 从 ModelScope 拉 DuReader 检索榜 dev 并裁剪成评测集。

为什么用它（相比 CMRC）：CMRC 的短段落对词面检索太容易（实测 evidence 95%），
校准与阈值都失去了分辨率。DuReader 检索榜是**真实用户问句**（中位 9 字）
+ **人工 hard negative**（每题中位 46 条，词面高度接近），并且一个问句有多条
正例（中位 3 条）——难度跨度和金标厚度都够。

输入：`AI-ModelScope/dureader-retrieval-ranking` 的 `dev.jsonl.gz`（Apache-2.0，35.6MB，2000 问）
      行形状：{"query_id", "query", "positive_passages":[{docid,text}], "negative_passages":[...]}

输出（与 prep_cmrc.py 同格式，直接接 CUMULUS_REALDATA=local）：
    corpus.jsonl   {"id","title","body"}
    items.jsonl    {"id","question","answer","gold_ids"}   多金标（任一命中即算）
    manifest.json  抽样口径 / 指纹 / 缺口

难度旋钮：`--negatives-per-query`（0 = 全用）。负例是人工 hard negative，
用得越多越难；用它把整体命中率调到 30–70% 的"校准可用区"。

用法：
    python3 scripts/prep_dureader.py --download --out ~/datasets/dureader/calib \\
        --queries 500 --negatives-per-query 8
"""

import argparse
import gzip
import hashlib
import json
import os
import random
import sys
import urllib.request

MS_HOST = "https://www.modelscope.cn/api/v1/datasets/AI-ModelScope/dureader-retrieval-ranking/repo"
RAW_PATH = "dev.jsonl.gz"


def hash12(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:12]


def download(dest_dir: str) -> str:
    os.makedirs(dest_dir, exist_ok=True)
    dest = os.path.join(dest_dir, RAW_PATH)
    if os.path.exists(dest) and os.path.getsize(dest) > 1_000_000:
        print(f"raw: 已有 {dest}（{os.path.getsize(dest)/1e6:.1f} MB），跳过下载")
        return dest
    url = f"{MS_HOST}?Revision=master&FilePath={RAW_PATH}"
    print(f"raw: 下载 {url}")
    with urllib.request.urlopen(url, timeout=600) as r, open(dest, "wb") as f:
        while True:
            chunk = r.read(1 << 20)
            if not chunk:
                break
            f.write(chunk)
    print(f"raw: 写入 {dest}（{os.path.getsize(dest)/1e6:.1f} MB）")
    return dest


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--raw", default="", help="dev.jsonl.gz 路径")
    ap.add_argument("--download", action="store_true", help="从 ModelScope 拉原始文件")
    ap.add_argument("--out", required=True)
    ap.add_argument("--queries", type=int, default=500, help="问句数（0 = 全量 2000）")
    ap.add_argument("--negatives-per-query", type=int, default=0, help="每题取多少 hard negative（0 = 全用）")
    ap.add_argument("--max-items", type=int, default=0, help="题数上限（0 = 不限）")
    ap.add_argument("--seed", type=int, default=20261008)
    args = ap.parse_args()

    raw = args.raw
    if not raw:
        candidate = os.path.join(args.out, RAW_PATH)
        raw = candidate if os.path.exists(candidate) else ("download" if args.download else "")
    if raw == "download" or (args.download and not os.path.exists(raw)):
        raw = download(args.out)
    if not raw or not os.path.exists(raw):
        print("raw: 找不到原始文件，用 --raw 指定或加 --download", file=sys.stderr)
        return 2

    rows = []
    opener = gzip.open if raw.endswith(".gz") else open
    with opener(raw, "rt", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    print(f"raw: {raw} → {len(rows)} 问")

    rng = random.Random(args.seed)
    order = list(range(len(rows)))
    rng.shuffle(order)
    keep = order if args.queries <= 0 else order[: args.queries]

    docs, items, seen = [], [], set()
    dropped_no_positive = 0
    for idx in sorted(keep):
        r = rows[idx]
        q = (r.get("query") or "").strip()
        if not q:
            continue
        pos, neg = [], []
        for p in r.get("positive_passages") or []:
            text = (p.get("text") or "").strip()
            if text and p.get("docid"):
                pos.append((p["docid"], text))
        negs = [p for p in (r.get("negative_passages") or [])]
        rng.shuffle(negs)
        if args.negatives_per_query > 0:
            negs = negs[: args.negatives_per_query]
        for p in negs:
            text = (p.get("text") or "").strip()
            if text and p.get("docid"):
                neg.append((p["docid"], text))
        if not pos:
            dropped_no_positive += 1
            continue  # 没有正例就评不了命中（dev 里有少数这种问句）
        # 语料 = 本问的正例 + 抽到的 hard negative（跨问去重）
        gold_ids = []
        for did, text in pos + neg:
            if did not in seen:
                seen.add(did)
                docs.append({"id": did, "title": "", "body": text})
            if (did, text) in pos:
                gold_ids.append(did)
        items.append({
            "id": str(r.get("query_id", idx)),
            "question": q,
            # 规则臂在这套数据上没有口径（正例是整段检索文本，不是短答案）——
            # 与 cn-law 同款的协议边界，记在 manifest 里，指标看证据命中与判官。
            "answer": pos[0][1][:120],
            "gold_ids": gold_ids,
        })

    docs.sort(key=lambda d: d["id"])
    items.sort(key=lambda x: x["id"])
    if args.max_items > 0 and len(items) > args.max_items:
        items = items[: args.max_items]

    os.makedirs(args.out, exist_ok=True)
    with open(os.path.join(args.out, "corpus.jsonl"), "w", encoding="utf-8") as f:
        for d in docs:
            f.write(json.dumps(d, ensure_ascii=False) + "\n")
    with open(os.path.join(args.out, "items.jsonl"), "w", encoding="utf-8") as f:
        for it in items:
            f.write(json.dumps(it, ensure_ascii=False) + "\n")

    golds = {g for it in items for g in it["gold_ids"]}
    manifest = {
        "source": "ModelScope AI-ModelScope/dureader-retrieval-ranking dev.jsonl.gz · Apache-2.0",
        "seed": args.seed,
        "queries_requested": args.queries or len(rows),
        "queries": len(items),
        "dropped_no_positive": dropped_no_positive,
        "docs": len(docs),
        "negatives_per_query": args.negatives_per_query or "all",
        "avg_doc_chars": round(sum(len(d["body"]) for d in docs) / max(1, len(docs)), 1),
        "gold_in_corpus": golds <= {d["id"] for d in docs},
        "answer_protocol": "answer = 首个正例前 120 字（规则臂无口径，看 evidence/judge）",
        "corpus_sha": hashlib.sha256("".join(d["id"] for d in docs).encode()).hexdigest()[:16],
        "items_sha": hashlib.sha256("".join(i["id"] for i in items).encode()).hexdigest()[:16],
    }
    with open(os.path.join(args.out, "manifest.json"), "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)
    print(json.dumps(manifest, ensure_ascii=False, indent=2))
    print(f"写成：{args.out}/corpus.jsonl + items.jsonl")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())