#!/usr/bin/env python3
"""prep_cmrc.py —— 从 ModelScope 拉 CMRC 2018 并裁剪成小语料评测集。

为什么是 CMRC：中文机器阅读理解，每篇 passage 是一段小文档（中位 454 字），
每题带金标段落（`context_id`）与金标答案（答案原文 + answer_start）。
裁剪后正好是 cumulus-next 评测要的形状：
    corpus.jsonl   {"id","title","body"}        检索宇宙
    items.jsonl    {"id","question","answer","gold_ids"}   题集（金标 = 段落 id）

裁剪规则（全部确定性：固定种子、同参数同输出）：
  - 抽 --docs 篇 context 当语料（0 = 全量）；
  - 题集只取落在语料内的 context 的问答 —— 金标必在语料里（否则不是
    检索失败，是数据错）；
  - 每题取第一条答案作文本，其余答案进 alt_answers（判官/人工复核用）；
  - 文档 id = sha256(正文)[:12]（与 evaldata 的内容寻址同款：同内容同 id）。

用法：
    python3 scripts/prep_cmrc.py --out /tmp/cmrc-small --docs 400 --max-items 200
    python3 scripts/prep_cmrc.py --download --out ~/datasets/cmrc-small

数据来源：ModelScope `OmniData/CMRC`（CC BY-SA 4.0），原始出处 CMRC 2018。
"""

import argparse
import hashlib
import json
import os
import random
import sys
import urllib.request

MS_HOST = "https://www.modelscope.cn/api/v1/datasets/OmniData/CMRC/repo"
RAW_PATH = "raw/cmrc2018/data/cmrc2018_dev.json"


def hash12(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:12]


def download(dest_dir: str) -> str:
    os.makedirs(dest_dir, exist_ok=True)
    dest = os.path.join(dest_dir, os.path.basename(RAW_PATH))
    if os.path.exists(dest) and os.path.getsize(dest) > 1_000_000:
        print(f"raw: 已有 {dest}（{os.path.getsize(dest)/1e6:.1f} MB），跳过下载")
        return dest
    url = f"{MS_HOST}?Revision=master&FilePath={RAW_PATH}"
    print(f"raw: 下载 {url}")
    with urllib.request.urlopen(url, timeout=180) as r, open(dest, "wb") as f:
        f.write(r.read())
    print(f"raw: 写入 {dest}（{os.path.getsize(dest)/1e6:.1f} MB）")
    return dest


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--raw", default="", help="cmrc2018_dev.json 路径（不给则用 --download 或默认缓存在 --out 下）")
    ap.add_argument("--download", action="store_true", help="从 ModelScope 拉原始文件")
    ap.add_argument("--out", required=True, help="输出目录")
    ap.add_argument("--docs", type=int, default=0, help="语料文档数（0 = 全量 848 篇）")
    ap.add_argument("--max-items", type=int, default=0, help="题数上限（0 = 不限）")
    ap.add_argument("--seed", type=int, default=20261008, help="抽样种子（固定种子 = 可复现）")
    ap.add_argument("--distractor-file", default="", help="干扰段落的来源文件（如 cmrc2018_train.json）")
    ap.add_argument("--distractors", type=int, default=0, help="从干扰文件另抽多少篇纯干扰文档（无题）")
    args = ap.parse_args()

    raw = args.raw
    if not raw:
        candidate = os.path.join(args.out, os.path.basename(RAW_PATH))
        raw = candidate if os.path.exists(candidate) else ("download" if args.download else "")
    if raw == "download" or (args.download and not os.path.exists(raw)):
        raw = download(args.out)
    if not raw or not os.path.exists(raw):
        print("raw: 找不到原始文件，用 --raw 指定路径或加 --download", file=sys.stderr)
        return 2

    with open(raw, encoding="utf-8") as f:
        records = json.load(f)
    print(f"raw: {raw} → {len(records)} 篇 context")

    rng = random.Random(args.seed)
    order = list(range(len(records)))
    rng.shuffle(order)
    keep = order if args.docs <= 0 else order[: args.docs]
    keep_set = set(keep)

    docs, items = [], []
    for idx in sorted(keep_set):          # 排序：输出与抽样顺序无关，只与集合有关
        rec = records[idx]
        body = (rec.get("context_text") or "").strip()
        if not body:
            continue
        did = hash12(body)
        docs.append({"id": did, "title": (rec.get("title") or "").strip(), "body": body})
        for qa in rec.get("qas") or []:
            # 这一版 CMRC 的 answers 是字符串列表；原版是 [{"text","answer_start"}]。
            # 两种都认，免得换一个镜像就崩。
            raw_answers = qa.get("answers") or []
            answers = []
            for a in raw_answers:
                if isinstance(a, dict):
                    text = a.get("text", "")
                elif isinstance(a, str):
                    text = a
                else:
                    text = "" if a is None else str(a)  # 脏标量（NaN 等）：当没有
                text = text.strip()
                if text and text.lower() != "nan":
                    answers.append(text)
            question = (qa.get("query_text") or "").strip()
            if not question or not answers:
                continue
            items.append({
                "id": qa.get("query_id") or hash12(question),
                "question": question,
                "answer": answers[0],
                "alt_answers": answers[1:],
                "gold_ids": [did],
            })

    items.sort(key=lambda x: x["id"])
    if args.max_items > 0 and len(items) > args.max_items:
        # 按 context 轮转取，保证题型分散（不是取 id 最小的那一坨）
        per_doc = {}
        for it in items:
            per_doc.setdefault(it["gold_ids"][0], []).append(it)
        picked, round_i = [], 0
        while len(picked) < args.max_items:
            added = False
            for did in sorted(per_doc):
                bucket = per_doc[did]
                if round_i < len(bucket):
                    picked.append(bucket[round_i])
                    added = True
                    if len(picked) >= args.max_items:
                        break
            if not added:
                break
            round_i += 1
        items = sorted(picked, key=lambda x: x["id"])

    os.makedirs(args.out, exist_ok=True)
    # 纯干扰段落：难度控制旋钮。校准集必须有难度跨度——全对或全错的
    # 集合上，置信度分桶没有分辨率，保序回归退化成一个台阶，τ₀ 也就
    # 无从谈起（首跑实测：400 篇小语料 96.5% 命中 → 分桶全是 1.00）。
    if args.distractors > 0:
        if not args.distractor_file:
            print("distractors: 给了 --distractors 就要给 --distractor-file", file=sys.stderr)
            return 2
        with open(args.distractor_file, encoding="utf-8") as f:
            pool = json.load(f)
        have = {d["id"] for d in docs}
        idxs = list(range(len(pool)))
        random.Random(args.seed + 1).shuffle(idxs)
        added = 0
        for i in idxs:
            if added >= args.distractors:
                break
            body = (pool[i].get("context_text") or "").strip()
            if not body:
                continue
            did = hash12(body)
            if did in have:
                continue
            have.add(did)
            docs.append({"id": did, "title": (pool[i].get("title") or "").strip(), "body": body})
            added += 1
        docs.sort(key=lambda d: d["id"])
        print(f"distractors: +{added} 篇（来自 {os.path.basename(args.distractor_file)}），语料共 {len(docs)} 篇")
    corpus_path = os.path.join(args.out, "corpus.jsonl")
    items_path = os.path.join(args.out, "items.jsonl")
    with open(corpus_path, "w", encoding="utf-8") as f:
        for d in docs:
            f.write(json.dumps(d, ensure_ascii=False) + "\n")
    with open(items_path, "w", encoding="utf-8") as f:
        for it in items:
            f.write(json.dumps(it, ensure_ascii=False) + "\n")

    gold_in_corpus = {it["gold_ids"][0] for it in items} <= {d["id"] for d in docs}
    avg_len = sum(len(d["body"]) for d in docs) / max(1, len(docs))
    manifest = {
        "source": "ModelScope OmniData/CMRC (cmrc2018_dev.json) · CC BY-SA 4.0",
        "seed": args.seed,
        "docs": len(docs),
        "items": len(items),
        "avg_doc_chars": round(avg_len, 1),
        "gold_in_corpus": gold_in_corpus,
        "corpus_sha": hashlib.sha256("".join(sorted(d["id"] for d in docs)).encode()).hexdigest()[:16],
        "items_sha": hashlib.sha256("".join(sorted(i["id"] for i in items)).encode()).hexdigest()[:16],
    }
    with open(os.path.join(args.out, "manifest.json"), "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)

    print(json.dumps(manifest, ensure_ascii=False, indent=2))
    print(f"写成：{corpus_path} / {items_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())