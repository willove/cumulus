#!/usr/bin/env bash
# selfplay-probe —— 自博弈奖励验证（认知引擎 Phase 0）。
#
# 要回答的问题（Sutton《苦涩的教训》第 2 要点 → R2 规则）：**稳定性能否当
# 奖励信号用**。做法：冻结集每题生成 K=2 个保义改写（扰动），在同一库上
# （生产形态：簇学习在场，顺序运行）各跑一次完整检索，量每个题的引用集
# 稳定性（逐对 Jaccard 均值）与模式/停止稳定性，然后对齐原始运行的判官
# 正确率——
#
#   稳定性与正确率分离（对的题稳定、错的题摇摆）⇒ 免费稠密伪奖励成立，
#   认知引擎有了地基；
#   不分离 ⇒ 先修判官（噪声地板 ±5-6/30 已实测），不要在流沙上盖楼。
#
# 预登记判据：正确组与错误组的平均引用稳定性差 ≥0.15 且方向在前/后两半
# 题上一致。record-only。
#
# 用法：scripts/selfplay-probe.sh [N]        # 默认 30 题
set -eu
cd "$(dirname "$0")/.."
N="${1:-30}"
K=2
SRC="${SET_DIR:-$(pwd)/var/semhead-ab}"   # 冻结集；SET_DIR 覆盖（跨语料复验：诗歌集等）
# 状态目录随冻结集分家：不同语料的复验互不覆盖、互不误续跑。
STATE="$(pwd)/var/selfplay-probe-$(basename "$SRC")"
STORE="$STATE/data"
ask="$STATE/cumulus-cluster"
mkdir -p "$STATE"
go build -o "$ask" ./cmd/cumulus-cluster
export CLUS_EMBED=minilm

# ── 1. 扰动生成：每题一次调用要 K 个保义改写（端点配置沿用套件 .env）──
if [ ! -s "$STATE/items-p1.jsonl" ] || [ ! -s "$STATE/items-p2.jsonl" ]; then
	python3 - "$SRC/items.jsonl" "$STATE" "$K" <<'PY'
import json, os, re, sys, urllib.parse, urllib.request

items_path, state, k = sys.argv[1], sys.argv[2], int(sys.argv[3])

def load_env(path):
    env = {}
    if not os.path.exists(path):
        return env
    for line in open(path, encoding="utf-8"):
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        kk, v = line.split("=", 1)
        env[kk.strip()] = v.strip().strip('"').strip("'")
    return env

env = load_env(".env")
base = os.environ.get("LLM_BASE_URL") or env.get("LLM_BASE_URL") or ""
key = os.environ.get("LLM_API_KEY") or env.get("LLM_API_KEY") or ""
model = (os.environ.get("LLM_CHAT_MODEL") or env.get("LLM_CHAT_MODEL")
         or env.get("LLM_MODEL_NAME") or "")
if not (base and key and model):
    raise SystemExit("selfplay: endpoint not configured (need AIGATE_*/LLM_* base/key/model)")

def chat(sys_msg, user_msg, retries=2):
    body = json.dumps({"model": model, "messages": [
        {"role": "system", "content": sys_msg},
        {"role": "user", "content": user_msg}], "temperature": 0.4}).encode()
    last = None
    for _ in range(retries + 1):
        req = urllib.request.Request(
            base.rstrip("/") + "/chat/completions", data=body,
            headers={"Content-Type": "application/json", "Authorization": "Bearer " + key})
        try:
            with urllib.request.urlopen(req, timeout=120) as r:
                out = json.load(r)
            return out["choices"][0]["message"]["content"]
        except Exception as e:  # noqa: BLE001 - dev tool, retry
            last = e
    raise RuntimeError(f"chat failed after {retries + 1} tries: {last}")

SYS = "你是检索系统的查询改写器。改写必须保持信息需求完全不变，只换措辞与语序，不得增删任何事实点。"
USR = ("把下面的问题改写成 %d 个不同措辞的版本（信息需求完全一致）。"
       "只输出 %d 行，每行一个改写，不要编号不要解释。\n问题：%s")

items = [json.loads(l) for l in open(items_path, encoding="utf-8")]
outs = [[] for _ in range(k)]
for it in items:
    raw = chat(SYS, USR % (k, k, it["query"]))
    # 推理端点会把思考漏进 content：剥 <think> 块，只留其后的正文。
    if "</think>" in raw:
        raw = raw.split("</think>", 1)[1]
    raw = re.sub(r"<think>[\s\S]*?</think>", "", raw)
    lines = [re.sub(r"^[\d０-９]+[.、）)\s]+", "", ln).strip()
             for ln in raw.strip().splitlines() if ln.strip()]
    # 正向过滤：改写必须与原问共享 >=3 个字符（保义的证据），且像一句话
    # 问题（长度带 + 非元话语）——I need to / Let me 类泄漏在此被拒收。
    def plausible(ln):
        shared = len(set(ln) & set(it["query"]))
        return shared >= 3 and 6 <= len(ln) <= 120 and not re.match(
            r"^(I |Let me|The |Okay|好的| Sure|Certainly|As requested)", ln)
    got = [ln for ln in lines if plausible(ln)][:k]
    while len(got) < k:  # 模型少给就重复原句（记为“零扰动”参与统计）
        got.append(it["query"])
    for i in range(k):
        outs[i].append(dict(it, id=it["id"], query=got[i]))
for i in range(k):
    with open(os.path.join(state, "items-p%d.jsonl" % (i + 1)), "w", encoding="utf-8") as f:
        for it in outs[i]:
            f.write(json.dumps(it, ensure_ascii=False) + "\n")
print("selfplay: perturbations written x%d" % k)
PY
fi

# ── 2. 新库 + 冻结语料（同温纪律）──
if [ ! -d "$STORE" ]; then
	"$ask" -data "$STORE" ensure
	"$ask" -data "$STORE" ingest-jsonl -file "$SRC/corpus.jsonl" -job selfplay
	"$ask" -data "$STORE" ensure -embed >/dev/null
	echo "selfplay: fresh store built"
fi

# ── 3. 顺序四跑：原始（带判官）→ p1/p2（无判官，同库生产形态）──
run_arm() { # name file judge
	local have=0
	[ -s "$STATE/r$1.jsonl" ] && have="$(wc -l < "$STATE/r$1.jsonl" | tr -d ' ')"
	if [ "$have" -lt "$N" ]; then
		local args=""
		[ "$3" = "judge" ] && args="-judge"
		"$ask" -data "$STORE" eval-run -file "$2" -out "$STATE/r$1.jsonl" \
			$args -tag "selfplay-$1" -limit "$((N - have))" > "$STATE/report-$1.json"
		echo "selfplay: r$1 done"
	else
		echo "selfplay: r$1 already complete"
	fi
}
run_arm 0 "$SRC/items.jsonl" judge
run_arm 1 "$STATE/items-p1.jsonl" nojudge
run_arm 2 "$STATE/items-p2.jsonl" nojudge

# ── 4. 结算：稳定性 × 正确率 ──
python3 - "$STATE" <<'PY'
import json, sys
state = sys.argv[1]

def load(name):
    return {r["id"]: r for r in map(json.loads, open(f"{state}/{name}", encoding="utf-8"))}

r0, r1, r2 = load("r0.jsonl"), load("r1.jsonl"), load("r2.jsonl")

def cites(r):
    return frozenset(r.get("cites") or [])

def jac(a, b):
    if not a and not b:
        return 1.0
    u = len(a | b)
    return len(a & b) / u if u else 0.0

rows = []
for i in sorted(r0):
    if i not in r1 or i not in r2:
        continue
    cs = [cites(r0[i]), cites(r1[i]), cites(r2[i])]
    stab = (jac(cs[0], cs[1]) + jac(cs[0], cs[2]) + jac(cs[1], cs[2])) / 3.0
    modes = {r0[i].get("mode"), r1[i].get("mode"), r2[i].get("mode")}
    rows.append({"id": i, "stab": stab, "ok": bool((r0[i].get("eval") or {}).get("correct")),
                 "mode_stable": len(modes) == 1,
                 "modes": ",".join(sorted(m for m in modes if m))})

ok = [r["stab"] for r in rows if r["ok"]]
bad = [r["stab"] for r in rows if not r["ok"]]
def stats(xs):
    xs = sorted(xs)
    return (len(xs), xs[len(xs)//2] if xs else 0, sum(xs)/len(xs) if xs else 0)
n1, m1, a1 = stats(ok)
n2, m2, a2 = stats(bad)
print(f"== selfplay stability vs correctness (n={len(rows)}) ==")
print(f"correct   : n={n1} median={m1:.2f} mean={a1:.2f}")
print(f"wrong     : n={n2} median={m2:.2f} mean={a2:.2f}")
print(f"gap(mean) = {a1-a2:+.2f}   [pre-registered bar: >= +0.15]")
half = len(rows)//2
for name, seg in (("first-half", rows[:half]), ("second-half", rows[half:])):
    o = [r["stab"] for r in seg if r["ok"]]; b = [r["stab"] for r in seg if not r["ok"]]
    d = (sum(o)/len(o) if o else 0) - (sum(b)/len(b) if b else 0)
    print(f"  {name}: gap={d:+.2f}")
ms = sum(1 for r in rows if r["mode_stable"])
print(f"mode-stable (FAST/DEEP across 3 runs): {ms}/{len(rows)}")
for r in rows:
    print(f"  {r['id']} ok={int(r['ok'])} stab={r['stab']:.2f} modes={r['modes']}")
PY
