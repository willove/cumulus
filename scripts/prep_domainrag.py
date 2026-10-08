#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""prep_domainrag.py — 把 DomainRAG 题集接成 cumulus 的评测输入。

来源：github.com/ShootingWong/DomainRAG（arXiv 2406.05654，中国人民大学公开
网页构建的**领域**中文 RAG 基准），题集与评测代码在仓库里；官方 corpus 在
Google Drive（本机不通，**题集自带正例文档正文 223 篇，够建小语料**）。

为什么用它而不是 DuReader 做校准主集：DuReader 的语料是网页噪声碎片，
DomainRAG 是**领域结构化文档**（招生/院系/专业页面），形态更接近"本地
知识库"。更关键的是它的**七类任务正好对着我们系统的缺口**：

    basic_qa        单跳抽取        → 基线
    multidoc_qa     多文档综合      → 我们只有单轮 BM25 多窗（已知缺口）
    structured_qa   结构化表格/HTML → 法条与表格抽取
    faithful_qa     **反事实**      → 幻觉/忠实性（counterfact 必须判错）
    time_sensitive  时效（date）    → 陈旧答案（研究线的 OUTDATED 类）
    conversation_qa 多轮            → 单轮 runner 不公平，**默认不导入**
    noisy_qa        噪声变体        → 鲁棒性（basic 的加噪版，另切）

输出（每个任务一个目录，与 prep_cmrc/prep_dureader 同形）：
    <task>/corpus.jsonl   {"id","title","body"}          题集自带正例正文
    <task>/items.jsonl    {"id","question","answer","answers","gold_ids",...}
    <task>/manifest.json  来源/许可/计数/协议

    python3 scripts/prep_domainrag.py --out ~/datasets/domainrag/all
    python3 scripts/prep_domainrag.py --out ~/datasets/domainrag/basic \
        --tasks basic --distractors 400 --seed 20261008
"""
import argparse
import hashlib
import json
import os
import random
import sys
import urllib.request
import zipfile

REPO_ZIP = "https://github.com/ShootingWong/DomainRAG/archive/refs/heads/main.zip"
# 任务名 → 题集文件。conversation 默认不导（需要多轮历史，单轮 runner 不公平）。
TASKS = {
    "basic": "BCM/labeled_data/extractive_qa/basic_qa.jsonl",
    "multidoc": "BCM/labeled_data/multi-doc_qa/multidoc_qa.jsonl",
    "structured": "BCM/labeled_data/structured_qa/structured_qa_twopositive.jsonl",
    "faithful": "BCM/labeled_data/faithful_qa/faithful_qa.jsonl",
    "time_sensitive": "BCM/labeled_data/time-sensitive_qa/time_sensitive.jsonl",
    "conversation": "BCM/labeled_data/conversation_qa/conversation_qa.jsonl",
    "noisy": "BCM/labeled_data/noisy_qa/noisy_qa_ver3.jsonl",
}


def fetch(dest_dir: str) -> str:
    dest = os.path.join(dest_dir, "DomainRAG-main")
    if os.path.isdir(dest):
        print(f"raw: 已有 {dest}（{os.path.getsize(dest)} 目录，跳过下载）")
        return dest
    zpath = os.path.join(dest_dir, "DomainRAG-main.zip")
    os.makedirs(dest_dir, exist_ok=True)
    print(f"raw: 下载 {REPO_ZIP}")
    with urllib.request.urlopen(REPO_ZIP, timeout=300) as r, open(zpath, "wb") as f:
        f.write(r.read())
    print(f"raw: {os.path.getsize(zpath)/1e6:.1f} MB → 解包")
    with zipfile.ZipFile(zpath) as z:
        z.extractall(dest_dir)
    return dest


def norm_answer(v):
    """answers 字段形状不一：[["汉语","法语"]] / ["1939年"] / 字符串。"""
    if v is None:
        return []
    if isinstance(v, str):
        return [v]
    out = []
    for item in v:
        if isinstance(item, str):
            out.append(item)
        elif isinstance(item, (list, tuple)):
            out.extend(str(x) for x in item)
    return [x for x in out if x]


def doc_id(ref: dict) -> str:
    body = ref.get("contents") or ""
    return hashlib.sha256(body.encode("utf-8")).hexdigest()[:12]


def collect_refs(rec: dict):
    """一条记录里所有带正文的文档引用（正例 / 转引 / 反事实）。"""
    refs = []
    pr = rec.get("positive_reference") or rec.get("positive_references") or []
    if isinstance(pr, dict):
        pr = [pr]
    refs += [r for r in pr if isinstance(r, dict) and r.get("contents")]
    refs += [r for r in (rec.get("referred_docs") or []) if isinstance(r, dict) and r.get("contents")]
    cf = rec.get("counterfact_reference")
    if isinstance(cf, dict) and cf.get("contents"):
        refs.append(cf)
    if isinstance(cf, list):
        refs += [r for r in cf if isinstance(r, dict) and r.get("contents")]
    # 去重（同 url/同正文只留一份）
    seen, out = set(), []
    for r in refs:
        k = doc_id(r)
        if k in seen:
            continue
        seen.add(k)
        out.append((k, r))
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--raw", default="", help="DomainRAG-main 目录（默认 ~/datasets/domainrag/raw）")
    ap.add_argument("--tasks", default="basic,multidoc,structured,faithful,time_sensitive")
    ap.add_argument("--corpus-from", default="", help="语料从哪个任务的引用并集建（默认=本任务）。"
                    "合并切片下用它避免跨任务噪声：basic 的候选池不该装 multidoc 的文档")
    ap.add_argument("--merge", action="store_true",
                    help="跨任务合并成一个切片（校准要一个题集；题号带 <task>: 前缀）")
    ap.add_argument("--distractors", type=int, default=0, help="每题掺多少干扰文档（0 = 不掺）")
    ap.add_argument("--max-items", type=int, default=0)
    ap.add_argument("--seed", type=int, default=20261008)
    args = ap.parse_args()

    raw = args.raw or os.path.join(os.path.expanduser("~"), "datasets/domainrag/raw")
    root = fetch(raw)
    tasks = [t.strip() for t in args.tasks.split(",") if t.strip()]
    for t in tasks:
        if t not in TASKS:
            print(f"unknown task {t}（可用：{', '.join(TASKS)}）", file=sys.stderr)
            return 2

    rng = random.Random(args.seed)
    merged_docs, merged_items, per_task = {}, [], {}
    # 先跑一遍 corpus_from 任务，只为把它的文档并集算出来（不落盘）
    corpus_docs = {}
    if args.corpus_from:
        cpath = os.path.join(root, TASKS[args.corpus_from])
        for line in open(cpath, encoding="utf-8"):
            if not line.strip():
                continue
            for did, ref in collect_refs(json.loads(line)):
                corpus_docs.setdefault(did, {"id": did, "title": ref.get("title", ""), "body": ref["contents"]})
    for task in tasks:
        path = os.path.join(root, TASKS[task])
        if not os.path.exists(path):
            print(f"skip {task}: 缺 {path}", file=sys.stderr)
            continue
        rows = [json.loads(l) for l in open(path, encoding="utf-8") if l.strip()]
        docs, items = {}, []
        used_ids, item_suffix = set(), 2
        dropped_no_gold = 0
        for i, rec in enumerate(rows):
            q = (rec.get("question") or "").strip()
            if not q:
                continue
            # 多轮：历史拼进问题（单轮 runner 只看一条问题）——默认不导这个任务，
            # 真要导必须承认它测的是"读得懂上文"，不是"检索得准"。
            hist = rec.get("history_qa") or []
            if hist:
                prev = "；".join((h.get("question") or "") for h in hist)
                q = f"（上文问过：{prev}）{q}"
            answers = norm_answer(rec.get("answers") or rec.get("answer"))
            refs = collect_refs(rec)
            gold_ids = [doc_id(r) for _, r in refs if r.get("id") == r.get("id")]  # 占位，下面按正例过滤
            gold_ids = []
            pr = rec.get("positive_reference") or rec.get("positive_references") or []
            if isinstance(pr, dict):
                pr = [pr]
            for r in pr:
                if isinstance(r, dict) and r.get("contents"):
                    gold_ids.append(doc_id(r))
            gold_ids = sorted(set(gold_ids))
            for did, r in refs:
                docs.setdefault(did, {"id": did, "title": r.get("title", ""), "body": r["contents"]})
            if not gold_ids:
                dropped_no_gold += 1
                continue  # 没有正例 docid 就没法判证据命中
            # 原始记录里 id 会撞（structured 任务 94 题里有 14 个重复号）：
            # 撞号在评测里是致命的（同一题两遍 = 指纹与统计都脏），这里显式去重。
            base_id = str(rec.get("id") or f"{task}-{i}")
            item_id = base_id
            while item_id in used_ids:
                item_id = f"{base_id}#{item_suffix}"
                item_suffix += 1
            used_ids.add(item_id)
            item = {
                "id": item_id,
                "question": q,
                "answer": answers[0] if answers else "",
                "gold_ids": gold_ids,
                "task": task,
            }
            if len(answers) > 1:
                item["answers"] = answers
            for k in ("date", "counterfact_answers", "commonsense"):
                if rec.get(k) not in (None, "", [], {}):
                    item[k] = rec[k]
            items.append(item)

        corpus = list(corpus_docs.values()) if corpus_docs else list(docs.values())
        for did, d in docs.items():
            corpus_docs.setdefault(did, d)
        # 难度旋钮：掺干扰文档（跨题去重采样）。金标永远在语料里。
        if args.distractors > 0 and len(items) > 1:
            pool = [d for d in corpus]
            extra, seen = [], set()
            need = args.distractors * max(1, len(items) // 2)
            for d in pool:
                if d["id"] in seen:
                    continue
                seen.add(d["id"])
                extra.append(d)
                if len(extra) >= need:
                    break
            rng.shuffle(extra)
            corpus = corpus + extra

        if args.merge:
            merged_docs.update(docs)
            merged_items.extend(items)
            per_task[task] = {"items": len(items), "docs": len(docs), "dropped_no_gold": dropped_no_gold}
            continue
        out_dir = os.path.join(args.out, task) if args.tasks != task and len(tasks) > 1 else args.out
        os.makedirs(out_dir, exist_ok=True)
        corpus.sort(key=lambda d: d["id"])
        items.sort(key=lambda x: x["id"])
        if args.max_items > 0:
            items = items[: args.max_items]
        with open(os.path.join(out_dir, "corpus.jsonl"), "w", encoding="utf-8") as f:
            for d in corpus:
                f.write(json.dumps(d, ensure_ascii=False) + "\n")
        with open(os.path.join(out_dir, "items.jsonl"), "w", encoding="utf-8") as f:
            for it in items:
                f.write(json.dumps(it, ensure_ascii=False) + "\n")
        manifest = {
            "source": "github.com/ShootingWong/DomainRAG（arXiv 2406.05654）· 题集来自仓库，"
                      "语料用题集自带正例正文；官方 corpus 在 Google Drive（本机不通）",
            "license": "仓库无 LICENSE 文件；语料为中国人民大学公开网页快照（事实性内容）",
            "task": task,
            "items": len(items),
            "docs": len(corpus),
            "dropped_no_gold": dropped_no_gold,
            "deduped_ids": item_suffix - 2,
            "distractors": args.distractors,
            "seed": args.seed,
            "answer_protocol": "answer = answers[0]（数据集给的是可接受答案列表）；"
                               "多轮任务的历史拼进问题文本",
            "fields": "items 保留 task/answers/counterfact_answers/date（评测器读前四个字段，"
                      "其余留给人工核对）",
        }
        with open(os.path.join(out_dir, "manifest.json"), "w", encoding="utf-8") as f:
            json.dump(manifest, f, ensure_ascii=False, indent=2)
        print(f"{task:14s} 题={len(items):3d} 文档={len(corpus):4d} 丢无金标={dropped_no_gold} → {out_dir}")
    if args.merge and merged_items:
        os.makedirs(args.out, exist_ok=True)
        corpus = [merged_docs[k] for k in sorted(merged_docs)]
        # 题号带任务前缀：分层报告（哪个任务拖后腿）靠前缀还原，
        # 评测器只读前四个字段，task 留在前缀里而不是白扔。
        items = [dict(it, id=f"{it.get('task','?')}:{it['id']}") for it in merged_items]
        items.sort(key=lambda x: x["id"])
        if args.max_items > 0:
            items = items[: args.max_items]
        with open(os.path.join(args.out, "corpus.jsonl"), "w", encoding="utf-8") as f:
            for d in corpus:
                f.write(json.dumps(d, ensure_ascii=False) + "\n")
        with open(os.path.join(args.out, "items.jsonl"), "w", encoding="utf-8") as f:
            for it in items:
                f.write(json.dumps(it, ensure_ascii=False) + "\n")
        manifest = {
            "source": "github.com/ShootingWong/DomainRAG（arXiv 2406.05654）· 跨任务合并",
            "license": "仓库无 LICENSE 文件；语料为中国人民大学公开网页快照（事实性内容）",
            "task": "merged:" + ",".join(tasks),
            "items": len(items),
            "docs": len(corpus),
            "per_task": per_task,
            "id_scheme": "<task>:<原始题号>（分层报告靠前缀）",
            "seed": args.seed,
            "answer_protocol": "answer = answers[0]；多轮任务未合并进来",
        }
        with open(os.path.join(args.out, "manifest.json"), "w", encoding="utf-8") as f:
            json.dump(manifest, f, ensure_ascii=False, indent=2)
        print(f"merged 题={len(items)} 文档={len(corpus)} → {args.out}")

    print(f"写成：{args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
