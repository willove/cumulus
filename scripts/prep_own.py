#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""prep_own.py — 把**你自己的语料 + 你自己的问句**接成 cumulus 评测输入。

为什么需要它：前面所有读数都来自外部数据集（CMRC / DuReader / DomainRAG），
它们的形态是"每题都该在库里查得到"。而真实个人知识库最缺的那一维——**该不该
检索**（通用常识、不该问、该拒答）——在那些数据上量不出来。这份脚本把自有语料
按同一套接口接进来，好让评测器跑一模一样的流程。

输入：
    1. 语料目录（.md/.txt/.json 任一子目录，或直接给一个目录，里面全是文档）
       → doc id 用**正文的 sha256 前 12 位**（与其他切片同一口径）
    2. 问句文件（JSONL，每行一题，见模板 questions-template.jsonl）：
       {"id","question","answer","gold_files","should_retrieve"}

       - gold_files： 金标文档的**文件名或相对路径**（脚本解析成 doc id——
         让人不用手算哈希，这是能不能让人愿意标的关键）
       - should_retrieve： true=答案该在库里 / false=不该走检索 / 省略=不参与该维

输出（与 prep_cmrc / prep_dureader 同形，可直接 CUMULUS_LOCAL_DIR 指过去）：
    <out>/corpus.jsonl  <out>/items.jsonl  <out>/manifest.json

    python3 scripts/prep_own.py --corpus ~/my-kb --questions ~/my-kb/questions.jsonl \
        --out ~/datasets/own/all
"""
import argparse
import hashlib
import json
import os
import re
import sys

DOC_EXT = (".md", ".txt", ".text", ".json", ".markdown")
SKIP_DIRS = {".git", "node_modules", ".obsidian", "__pycache__", ".venv"}


def read_doc(path: str):
    """读一个文档，返回 (title, body)。JSON 取其中的正文字段。"""
    try:
        raw = open(path, encoding="utf-8", errors="replace").read()
    except OSError:
        return None
    if path.endswith(".json"):
        try:
            d = json.loads(raw)
        except json.JSONDecodeError:
            return None
        if isinstance(d, dict):
            body = d.get("body") or d.get("text") or d.get("content") or d.get("contents") or ""
            title = d.get("title") or os.path.basename(path)
            return str(title), str(body)
        return os.path.basename(path), json.dumps(d, ensure_ascii=False)
    title = ""
    m = re.match(r"^#\s+(.+)$", raw.strip(), re.M)
    if m:
        title = m.group(1).strip()
    if not title:
        title = os.path.splitext(os.path.basename(path))[0]
    return title, raw


def walk_corpus(root: str):
    """走目录出 (relpath, docid, title, body)。"""
    docs = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS and not d.startswith(".")]
        for fn in sorted(filenames):
            if not fn.lower().endswith(DOC_EXT):
                continue
            full = os.path.join(dirpath, fn)
            rel = os.path.relpath(full, root)
            got = read_doc(full)
            if not got:
                continue
            title, body = got
            if len(body.strip()) < 20:
                continue  # 空文档不进语料（多半是索引/占位文件）
            docs.append((rel, hashlib.sha256(body.encode("utf-8")).hexdigest()[:12], title, body))
    return docs


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", required=True, help="语料目录")
    ap.add_argument("--questions", required=True, help="问句 JSONL")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    docs = walk_corpus(args.corpus)
    if not docs:
        print("语料目录里没有可用文档", file=sys.stderr)
        return 2
    # relpath → docid / title（宽松匹配：全名、去后缀、basename 都认）
    by_name: dict[str, str] = {}
    for rel, did, title, _ in docs:
        for key in {rel, os.path.splitext(rel)[0], os.path.basename(rel),
                    os.path.splitext(os.path.basename(rel))[0]}:
            by_name[key] = did

    items, missing, no_gold, should_no = [], [], [], 0
    seen = set()
    for ln, raw in enumerate(open(args.questions, encoding="utf-8"), 1):
        raw = raw.strip()
        if not raw or raw.startswith("//"):
            continue
        rec = json.loads(raw)
        q = (rec.get("question") or "").strip()
        if not q:
            print(f"第 {ln} 行没有 question，跳过", file=sys.stderr)
            continue
        item_id = str(rec.get("id") or f"q{ln}")
        if item_id in seen:
            print(f"重复题号 {item_id}，跳过", file=sys.stderr)
            continue
        seen.add(item_id)
        golds = []
        for name in rec.get("gold_files") or []:
            did = by_name.get(name) or by_name.get(str(name).strip())
            if did is None:
                missing.append(name)
                continue
            golds.append(did)
        sr = rec.get("should_retrieve")
        if sr is False:
            should_no += 1
        elif not golds:
            no_gold += 1
        items.append({
            "id": item_id,
            "question": q,
            "answer": (rec.get("answer") or "").strip(),
            "gold_ids": sorted(set(golds)),
            "should_retrieve": sr,
            "tag": rec.get("tag", ""),
        })

    os.makedirs(args.out, exist_ok=True)
    with open(os.path.join(args.out, "corpus.jsonl"), "w", encoding="utf-8") as f:
        for _, did, title, body in sorted(docs, key=lambda x: x[1]):
            f.write(json.dumps({"id": did, "title": title, "body": body}, ensure_ascii=False) + "\n")
    with open(os.path.join(args.out, "items.jsonl"), "w", encoding="utf-8") as f:
        for it in items:
            f.write(json.dumps(it, ensure_ascii=False) + "\n")

    manifest = {
        "source": f"自有语料 {args.corpus} + 自有问句 {args.questions}",
        "license": "自建（按你自己的使用范围）",
        "docs": len(docs),
        "items": len(items),
        "should_retrieve_false": should_no,
        "items_without_gold": no_gold,
        "unmatched_gold_files": sorted(set(missing)),
        "doc_id_scheme": "正文 sha256 前 12 位（与其他切片同口径）",
        "note": "gold_files 用文件名或相对路径指定金标文档；找不到的会列在"
                " unmatched_gold_files 里——**金标找不到是必须先解决的**（否则"
                " 证据命中这一列量的是别的东西）",
    }
    with open(os.path.join(args.out, "manifest.json"), "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)

    print(f"语料 {len(docs)} 篇 → {args.out}/corpus.jsonl")
    print(f"问句 {len(items)} 条（其中 {should_no} 条标注'不该检索'，{no_gold} 条无金标）→ items.jsonl")
    if missing:
        print(f"⚠️ 有 {len(set(missing))} 个 gold_files 在语料里找不到（证据命中会是假 0）:", file=sys.stderr)
        for m in sorted(set(missing))[:10]:
            print(f"   - {m}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
