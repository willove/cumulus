#!/usr/bin/env bash
# poetry-forge —— 在密封诗歌对抗集上注入伪造文档（语料可信度考场）。
#
# 为什么修订验证数据：两战（法条难负例/同题异作者）证明"各自为真"的语料
# 不构成 conflicts_with 要的"同一事实点相互否定"。本脚本在 var/poetry-
# adversarial 的密封集上加第三篇【伪本】：金标正文逐字保留、作者行换成
# 干扰作者——正文逐字相同而作者声明相反，是打分器 prompt 里教科书级的
# 同点异值矛盾。这一战 isolate 机制：若此形态仍不标矛盾，v3b 终局是能力
# 判决而非语料借口。
#
# 度量主轴：1 对 1 毒化在封闭语料内不可判真（诚实结果是存疑），所以成败
# 指标是失败形态（自信答错 vs 诚实存疑），出口分布仍是否决咬合的证据。
# 伪本只存在于本冻结集（var/），永不进入任何被服务的语料。
#
# 确定性：纯字符串手术（按 poemNNN/distNNN 配对），manifest 链到父集。
# 用法：scripts/poetry-forge.sh    # 读 var/poetry-adversarial → var/poetry-forged
set -eu
cd "$(dirname "$0")/.."
SRC="$(pwd)/var/poetry-adversarial"
STATE="$(pwd)/var/poetry-forged"
[ -s "$SRC/corpus.jsonl" ] && [ -s "$SRC/items.jsonl" ] || {
	echo "poetry-forge: $SRC lacks sealed set (run scripts/poetry-distractor.sh first)" >&2; exit 1; }
mkdir -p "$STATE"

python3 - "$SRC" "$STATE" <<'PY'
import hashlib, json, os, sys

src, state = sys.argv[1], sys.argv[2]

docs = {}
for line in open(os.path.join(src, "corpus.jsonl"), encoding="utf-8"):
    d = json.loads(line)
    docs[d["key"]] = d["text"]

out_docs, forged = {}, 0
for k, text in docs.items():
    out_docs[k] = text                       # 金标与同题干扰原样保留
    if not k.startswith("poem"):
        continue
    dk = "dist" + k[4:]
    # 伪本 = 金标全文 + 干扰作者的作者行。只在"全文："前的首段做一次
    # 作者替换，正文逐字不动——矛盾精确落在作者这一个事实点上。
    head, sep, body = text.partition("。全文：")
    if not sep:
        raise SystemExit(f"gold doc {k} lost its self-describing head")
    dist_head, _, _ = docs[dk].partition("。全文：")
    da = dist_head.split("作者：", 1)[1].split("。", 1)[0]   # 干扰作者
    fa = head.split("作者：", 1)[1].split("。", 1)[0]        # 金标作者
    if fa == da:
        raise SystemExit(f"{k}: gold and distractor share author {fa!r}")
    out_docs["forge" + k[4:]] = head.replace("作者：" + fa, "作者：" + da, 1) + sep + body
    forged += 1

items = [json.loads(l) for l in open(os.path.join(src, "items.jsonl"), encoding="utf-8")]

corpus_p = os.path.join(state, "corpus.jsonl")
items_p = os.path.join(state, "items.jsonl")
man_p = os.path.join(state, "manifest.sha256.json")
with open(corpus_p, "w", encoding="utf-8") as f:
    for key in sorted(out_docs):
        f.write(json.dumps({"key": key, "title": key, "text": out_docs[key]},
                           ensure_ascii=False) + "\n")
with open(items_p, "w", encoding="utf-8") as f:
    for it in items:
        f.write(json.dumps(it, ensure_ascii=False) + "\n")

def sha(p):
    return hashlib.sha256(open(p, "rb").read()).hexdigest()

parent = json.load(open(os.path.join(src, "manifest.sha256.json"), encoding="utf-8"))
with open(man_p, "w", encoding="utf-8") as f:
    json.dump({"kind": "poetry-same-title-forged", "parent_kind": parent["kind"],
               "parent_items_sha": parent["sha256"]["items.jsonl"],
               "items": len(items), "docs": len(out_docs), "forged": forged,
               "sha256": {"items.jsonl": sha(items_p), "corpus.jsonl": sha(corpus_p)}},
              f, ensure_ascii=False, indent=2)
    f.write("\n")
print(f"poetry forged set: items={len(items)} docs={len(out_docs)} (forged={forged}) — sealed {man_p}")
PY

# 完整性双验：伪本正文与金标逐字一致且仅作者行不同；每题都有伪本。
python3 - "$STATE" <<'PY'
import json, os, sys
state = sys.argv[1]
docs = {d["key"]: d["text"] for d in map(json.loads, open(os.path.join(state, "corpus.jsonl"), encoding="utf-8"))}
items = [json.loads(l) for l in open(os.path.join(state, "items.jsonl"), encoding="utf-8")]
ok_body = ok_distinct = 0
for it in items:
    n = it["id"][1:]
    gold, forge = docs["poem" + n], docs["forge" + n]
    gh, _, gb = gold.partition("。全文：")
    fh, _, fb = forge.partition("。全文：")
    ok_body += (gb == fb and gh.split("作者：")[0] == fh.split("作者：")[0])
    ok_distinct += (gh.split("作者：", 1)[1].split("。")[0] != fh.split("作者：", 1)[1].split("。")[0])
print(f"integrity: body-identical-author-swapped {ok_body}/{len(items)}, authors-distinct {ok_distinct}/{len(items)}")
if ok_body != len(items) or ok_distinct != len(items):
    raise SystemExit("forgery integrity FAILED")
PY
