#!/usr/bin/env bash
# poetry-distractor —— 同题异作者对抗冻结集（c_d 考场）建集器。
#
# 为什么是这个集：cn-law 的难负例是【真法条】——检索干扰项而非矛盾，
# conflictdims A/B 证明 dims 打分器在其上不标矛盾是正确行为。v3b 否决的
# 考场需要语料里两篇文档【对同一问题给出不同答案】：全唐诗/全宋诗同题
# 异作者诗歌正是这个结构——《蝉》同时存在于虞世南/骆宾王/李商隐名下，
# "《蝉》的作者是谁"在语料中有两个词面都命中、答案不同的文档——论文
# 对抗题类的对应物。
#
# 全程确定性（无随机数）：抽样=对标题做 sha256(SEED|title) 键排序取前 N；
# 金标=语料内最高产作者（稳定的 canonical 代理），干扰=次高产的其他作者。
# 文档自描述（作者：行）保证答案串在金标文档中逐字可 EM。manifest 密封
# （gen_questions 纪律：冻结一次，只有故意才重建）。
#
# 用法：scripts/poetry-distractor.sh [N]     # 默认 30 题
set -eu
cd "$(dirname "$0")/.."
POETRY_ROOT="${POETRY_ROOT:-$HOME/datasets/chinese-poetry-master}"
N="${1:-30}"
STATE="$(pwd)/var/poetry-adversarial"
mkdir -p "$STATE"

[ -d "$POETRY_ROOT" ] || { echo "poetry-distractor: $POETRY_ROOT not found" >&2; exit 1; }

python3 - "$POETRY_ROOT" "$N" "$STATE" <<'PY'
import hashlib, json, os, sys
from collections import Counter, defaultdict

root, n_items, state = sys.argv[1], int(sys.argv[2]), sys.argv[3]
SEED = 20260929

def load_poems(r):
    for dirpath, _dirs, files in os.walk(r):
        for fn in sorted(files):
            if not (fn.startswith("poet.") and fn.endswith(".json")):
                continue
            try:
                with open(os.path.join(dirpath, fn), encoding="utf-8") as f:
                    for p in json.load(f):
                        a = (p.get("author") or "").strip()
                        t = (p.get("title") or "").strip()
                        paras = p.get("paragraphs") or []
                        if a and t and paras:
                            yield a, t, "".join(paras)
            except Exception as e:  # noqa: BLE001 - 一个坏文件不能杀掉整个集
                print(f"skip {fn}: {e}", file=sys.stderr)

poems = list(load_poems(root))
author_freq = Counter(a for a, _, _ in poems)
by_title = defaultdict(list)
for a, t, text in poems:
    by_title[t].append((a, text))

groups = []
for title, entries in by_title.items():
    if not title:
        continue
    authors = {a for a, _ in entries}
    if len(authors) < 2:
        continue
    # 确定性排序：语料频次降序，再按名字——高产作者作 canonical 代理。
    ranked = sorted(authors, key=lambda a: (-author_freq[a], a))
    gold_a, dist_a = ranked[0], ranked[1]
    gold = next(text for a, text in entries if a == gold_a)
    dist = next(text for a, text in entries if a == dist_a)
    if len(gold) < 20 or len(dist) < 20:
        continue
    groups.append((title, gold_a, gold, dist_a, dist))

# 确定性伪洗牌：sha256 键排序替代随机数，重跑逐位复现。
groups.sort(key=lambda g: hashlib.sha256(
    (str(SEED) + "|" + g[0]).encode("utf-8")).hexdigest())
picked = groups[:n_items]
if len(picked) < n_items:
    raise SystemExit(f"only {len(picked)} same-title groups available, want {n_items}")

items, docs = [], {}
for i, (title, ga, gtext, da, dtext) in enumerate(picked):
    pk, dk = "poem%03d" % i, "dist%03d" % i
    docs[pk] = "《%s》 作者：%s。全文：%s" % (title, ga, gtext)
    docs[dk] = "《%s》 作者：%s。全文：%s" % (title, da, dtext)
    items.append({"id": "p%03d" % i,
                  "query": "《%s》這首詩的作者是誰？" % title,
                  "answer": ga, "gold_sources": [pk]})

items_p = os.path.join(state, "items.jsonl")
corpus_p = os.path.join(state, "corpus.jsonl")
man_p = os.path.join(state, "manifest.sha256.json")
with open(items_p, "w", encoding="utf-8") as f:
    for it in items:
        f.write(json.dumps(it, ensure_ascii=False) + "\n")
with open(corpus_p, "w", encoding="utf-8") as f:
    for k, text in docs.items():
        f.write(json.dumps({"key": k, "title": k, "text": text}, ensure_ascii=False) + "\n")

def sha(p):
    return hashlib.sha256(open(p, "rb").read()).hexdigest()

with open(man_p, "w", encoding="utf-8") as f:
    json.dump({"kind": "poetry-same-title-adversarial", "seed": SEED,
               "items": len(items), "docs": len(docs),
               "corpus_root": os.path.abspath(root),
               "sha256": {"items.jsonl": sha(items_p), "corpus.jsonl": sha(corpus_p)}},
              f, ensure_ascii=False, indent=2)
    f.write("\n")
print("poetry adversarial set: items=%d docs=%d (gold+distractor per title) — sealed %s"
      % (len(items), len(docs), man_p))
PY
