#!/usr/bin/env bash
# probe_long.sh —— **长文档**探针：把"知识库真实形态"下的检索表现量出来。
#
# 为什么单独一个探针：现有全部读数来自短条文（每篇 3 条、共 7 篇），而个人知识库
# 的文档是长文——**答案通常埋在中段**。这里量三个数：
#
#   1. evidence  金标文档是否被检索到
#   2. window    **引用窗口里是否真的包含答案片段**（长文档最脆弱的一环）
#   3. refusal   语料外的问题是否拒答
#
# 用法：
#   DASHSCOPE_API_KEY=… bash scripts/probe_long.sh          # 真模型
#   bash scripts/probe_long.sh --offline                    # 离线合成
#
# 数据来自 scripts/prep_long.py（自造长文档；**不是基准**，只是把长文档这个形态
# 测出来）。
set -uo pipefail
cd "$(dirname "$0")/.."
export GOCACHE="$PWD/.gocache"

if ! command -v go >/dev/null 2>&1; then
  for cand in /usr/local/go/bin /opt/homebrew/bin /usr/local/bin "$HOME/go/bin" "$HOME/sdk"/go*/bin; do
    [[ -x "$cand/go" ]] && export PATH="$cand:$PATH" && break
  done
fi

OFFLINE=0
[[ "${1:-}" == "--offline" ]] && OFFLINE=1
DIR="${CUMULUS_LONG_DIR:-$HOME/datasets/long}"
PORT=${PORT:-19350}
KEY=sk-long-probe
WORK=$(mktemp -d /tmp/cumulus-long.XXXXXX)
BIN="$WORK/cumulus"
BASE="http://127.0.0.1:$PORT"
trap 'pkill -f "cumulus-long-probe" 2>/dev/null; rm -rf "$WORK"' EXIT

[[ -f "$DIR/corpus.jsonl" ]] || { echo "先造语料: python3 scripts/prep_long.py --out $DIR"; exit 2; }

SYNTH="-synth offline"
ENVV=()
if [[ $OFFLINE -eq 0 ]]; then
  [[ -z "${DASHSCOPE_API_KEY:-}" ]] && { echo "缺 DASHSCOPE_API_KEY（或加 --offline）"; exit 2; }
  SYNTH="-synth llm"
  ENVV=(CUMULUS_ZERO_WINDOW_ESCALATE=1)
fi

go build -o "$BIN" ./cmd/cumulus || exit 1
env CUMULUS_KEYS="probe=$KEY" ${ENVV[@]+"${ENVV[@]}"} \
  "$BIN" serve $SYNTH -listen "127.0.0.1:$PORT" -data "$WORK/data" -watch "$DIR" \
  >"$WORK/serve.log" 2>&1 &
# 等语料就位。**必须等篇数而不是只等端口**：health 在空语料下也返回 200，
# 于是"服务活着但一篇都没有"会被当成就绪——第一版脚本就这样跑出了 0/10 的
# 假读数（而手动起同样的服务是 9 篇）。
docs=0
for _ in $(seq 1 120); do
  docs=$(curl -sf "$BASE/v1/health" -H "X-Cumulus-Key: $KEY" 2>/dev/null |
    python3 -c 'import sys,json;print(json.load(sys.stdin).get("corpus_docs",0))' 2>/dev/null || echo 0)
  [[ "${docs:-0}" -ge 9 ]] && break
  sleep 0.5
done
if [[ "${docs:-0}" -lt 9 ]]; then
  echo "长文档未就位（${docs}/9 篇）——先看日志：" >&2
  tail -5 "$WORK/serve.log" >&2
  exit 1
fi
echo "长文档已就位：${docs} 篇（目录 $DIR，窗口宽 400 runes，文档 379–1733 字）"
# 自检：探针必须读到**自己那份**语料（金标 id 对不上时读数全是假的）。
python3 - "$DIR" "$BASE" "$KEY" <<'SELFPROBE'
import json, sys, urllib.request
dir_path, base, key = sys.argv[1], sys.argv[2], sys.argv[3]
docs = [json.loads(l) for l in open(f"{dir_path}/corpus.jsonl", encoding="utf-8")]
req = urllib.request.Request(f"{base}/v1/health", headers={"X-Cumulus-Key": key})
with urllib.request.urlopen(req, timeout=30) as r:
    got = json.load(r).get("corpus_docs", 0)
if got != len(docs):
    print(f"  ⚠ 语料对不上：磁盘 {len(docs)} 篇 vs 服务 {got} 篇（读数不可信）", file=sys.stderr)
    sys.exit(3)
print(f"  自检通过：磁盘与服务都是 {got} 篇")
SELFPROBE
echo

python3 - "$DIR" "$BASE" "$KEY" <<'PY'
import json, sys, urllib.request

dir_path, base, key = sys.argv[1], sys.argv[2], sys.argv[3]
items = [json.loads(l) for l in open(f"{dir_path}/items.jsonl", encoding="utf-8")]

def ask(q):
    req = urllib.request.Request(f"{base}/v1/qa",
        data=json.dumps({"question": q}).encode(),
        headers={"Content-Type": "application/json", "X-Cumulus-Key": key})
    with urllib.request.urlopen(req, timeout=120) as r:
        return json.load(r)

# 语料外题的判据：**不该给出实质答案**。
#
# 为什么不用 `refused` 当唯一判据（真跑教训）：模型对"证据只支持一部分"的题会给
# "现有证据仅支持 X，未提供 Y" 这种**诚实的不完整回答**，此时 refused=false——
# 那是**对的**行为，不是失败。用 refused 判会把"诚实"误判成"幻觉"，把"幻觉"
# 混进"拒绝"里。两个指标分开量：
#   - should_refuse：完全不该答（幻觉）
#   - honest_partial：明确说明了覆盖不到的部分（诚实，正确）
rows, ev_hit, win_hit, refused_ok, honest, halluc, answerable = [], 0, 0, 0, 0, 0, 0
for it in items:
    d = ask(it["question"])
    gold, ans = set(it.get("gold_ids") or []), (it.get("answer") or "")
    wins = d.get("windows") or []
    got = {w.get("source_id") for w in wins}
    cited = json.dumps(d.get("citations") or [], ensure_ascii=False)
    win_text = " ".join((w.get("text") or "") for w in wins)
    # 归一化后比较：模型/格式化可能引入换行、全角标点与半角混用，
    # 不归一化会把"答案就在窗口里"判成没命中（真跑踩过一次这样的假阴性）。
    def norm(x: str) -> str:
        return "".join(ch for ch in (x or "") if not ch.isspace()).replace("，", ",").replace("。", ".")
    ev = bool(gold & got) if gold else None
    wh = (norm(ans) in norm(win_text) or norm(ans) in norm(cited)) if ans else None
    if ans:
        answerable += 1
        ev_hit += 1 if ev else 0
        win_hit += 1 if wh else 0
    else:
        ans_txt = (d.get("answer") or "")
        # 三个互斥桶（顺序即优先级）：
        #   refused=True 或空答案 → 完整拒答（对的）
        #   答案里明说"未涉及/未提供/未规定/无法确定" → **诚实部分回答**（也是对的）
        #   其余 → 幻觉（给了实质答案却语料里没有）
        if d.get("refused") or not ans_txt.strip():
            refused_ok += 1
        elif any(k in ans_txt for k in ("未涉及", "未提供", "未说明", "未规定",
                                         "无法确定", "无法据此", "仅支持", "未给出")):
            honest += 1
        else:
            halluc += 1
    if wh is False:
        # 判失败的题把窗口逐条打出来：否则"明明手动能复现、脚本却判失败"这类事
        # 只能靠猜（真跑在这上面卡了很久）。
        print(f"    ↳ {it['id']} 未命中，打印窗口：")
        for w in wins[:6]:
            print(f"        {w.get('span')} 分={w.get('score',0):.1f} 含答案={norm(ans) in norm(w.get('text') or '')} | {(w.get('text') or '')[:26]}")
        print(f"        期望答案: {ans[:40]}")
    rows.append((it["id"], it["question"][:26], d.get("refused"), ev, wh, len(wins), (d.get("answer") or "")[:0]))

w = "{:<9}{:<28}{:>8}{:>10}{:>10}{:>7}"
print(w.format("id", "问题", "拒答", "金标命中", "窗口含答案", "窗数"))
for r in rows:
    print(w.format(r[0], r[1], str(r[2]),
                   "-" if r[3] is None else ("✓" if r[3] else "✗"),
                   "-" if r[4] is None else ("✓" if r[4] else "✗"), r[5]))
print()
if answerable:
    print(f"可答题 {answerable}：金标命中 {ev_hit}/{answerable} ({100*ev_hit//answerable}%) · "
          f"窗口含答案 {win_hit}/{answerable} ({100*win_hit//answerable}%)")
n_abs = len(items) - answerable
print(f"语料外 {n_abs}：完整拒答 {refused_ok} · 诚实部分回答 {honest} · 应无幻觉 {n_abs - refused_ok - honest}"
      f"（即幻觉 {refused_ok + honest - n_abs + n_abs} 题中的 {refused_ok} 题）" if False else
      f"语料外 {n_abs}：完整拒答 {refused_ok} · 诚实部分回答 {honest} · 幻觉（给了实质答案）{n_abs - refused_ok - honest}")
PY
