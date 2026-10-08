# 自有语料评测：怎么跑

外部数据集（CMRC / DuReader / DomainRAG）量的是"每题都该在库里查得到"的那种系统。
**你的语料不是这样**——它有相当比例的问句根本不需要检索（常识、不该问、该拒答），
而系统现在无论如何先检索再回答，于是自信地答错。这一维叫 `should_retrieve`，
在外部语料上量不出来（那些题几乎全是 true），只能用自己的问句集量。

## 三步

```bash
# 1. 语料：一个目录，里面是文档（.md/.txt/.json）
#    doc id = 正文 sha256 前 12 位（与其他切片同口径，你不用手算）

# 2. 问句：按 examples/questions-template.jsonl 填，每行一题
#    gold_files 用**文件名或相对路径**写金标文档（脚本会解析成 doc id）
#    should_retrieve：true=答案该在库里 / false=不该走检索 / 省略=不参与该维

# 3. 接入 + 跑
python3 scripts/prep_own.py --corpus ~/my-kb --questions ~/my-kb/questions.jsonl \
    --out ~/datasets/own/all
CUMULUS_REALDATA=local CUMULUS_LOCAL_DIR=~/datasets/own/all go run ./cmd/cumulus eval
```

## 读数怎么看

摘要末尾多两段：

```
gate[gate/answerable](decided=40 blocked=0 {ok:40})   ← 闸门跑了没、拦没拦、为什么
should-retrieve(标注=3 不该检索却检索了=0/1 该检索没检索=0)
```

- **`不该检索却检索了`**：标了 false 却还是走了检索 → 缺"该不该检索"这一层
  （系统该直接答或直接拒，而不是先检索）；
- **`该检索没检索`**：标了 true 却被闸门/路由拦下 → **拦过头**，比漏拦更隐蔽；
- **未标注的题不计入**（读数显示 `unannotated`）——不假装有这一维的读数。

## 金标找不到会被列出来

`manifest.json` 的 `unmatched_gold_files` 里是脚本没在语料里找到的金标文件名。
**这必须先解决**：金标文档不在语料里，证据命中那一列量的是别的东西（会恒为 0，
看着像"全错"，其实是标错了）。

## 建议的规模

20–50 题起步。这一维看的是**比例**（不该检索却检索了 X/Y），不是绝对数——
20 题就能看出"这层缺不缺"，50 题能看出缺多少。不用一次填 200 题。
