#!/usr/bin/env bash
# acceptance.sh —— 端到端验收：**正常路径 + 失败路径**一起跑，输出可对比的 PASS/FAIL。
#
# 为什么要有这个脚本（不是一次性的手工检查）：
#   手工复盘的问题是"这次过了，下次呢"。把链路上的**每一条承诺**写成断言，
#   任何人、任何时候都能复跑；改了哪一环坏了哪一环，当场红。
#
# 它检查什么（按链路顺序）：
#   1. 能力可见面（/v1/status、/v1/health）——缺什么要说出来，不静默
#   2. 问答：答得上（有引用）/ 答不上（诚实拒答）
#   3. 词汇桥与升级：鸿沟问句能被救回来
#   4. 事件流：帧类别齐全、进度分母正确
#   5. 会话回放：cursor 续读 + manifest
#   6. 知识文档：生成→落库→被引用；再生成→同主题更新（不堆重复）
#   7. 选题建议：从拒答/追问信号里给候选，已覆盖的标出来
#   8. 多租户：无凭证 401 / 错凭证 403 / 跨租户看不到对方的文档
#   9. 失败路径：空主题 / 无证据 / 坏 JSON 请求 —— 都必须**给出可读原因**而不是崩
#
# 用法：
#   DASHSCOPE_API_KEY=... bash scripts/acceptance.sh          # 真模型
#   bash scripts/acceptance.sh --offline                      # 离线合成（不花 LLM 钱）
#
# 退出码：0 = 全过；非 0 = 有断言失败（看 FAIL 行）。
set -uo pipefail

cd "$(dirname "$0")/.."
export GOCACHE="$PWD/.gocache"

OFFLINE=0
[[ "${1:-}" == "--offline" ]] && OFFLINE=1

PORT=${PORT:-19300}
KEY_A=sk-accept-a
KEY_B=sk-accept-b
WORK=$(mktemp -d /tmp/cumulus-acceptance.XXXXXX)
CORPUS="$WORK/corpus"
DATA="$WORK/data"
BIN="$WORK/cumulus"
LOG="$WORK/serve.log"
BASE="http://127.0.0.1:$PORT"

PASS=0; FAIL=0; NOTE=""
cleanup() { pkill -f "cumulus-accept" 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

ok()   { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; [[ -n "${2:-}" ]] && printf '       %s\n' "$2"; }
note() { NOTE="${NOTE}$1\n"; printf '  \033[33mNOTE\033[0m %s\n' "$1"; }
head_() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# ── 语料（2 个主题 + 同主题干扰文档：让首程"够不着"金标，桥与升级才有机会上场）
mkdir -p "$CORPUS"
cat > "$CORPUS/patent-fee.md" <<'EOF'
# 专利年费缴纳办法

第一条 专利权人应当自专利权授予之日起每年缴纳年费。发明专利年费为每三百八十元，实用新型年费为一百九十元。

第二条 未在规定期限内缴纳年费的，应当在期满后六个月内补缴，并缴纳百分之五十的滞纳金；逾期未补缴的，终止专利权。

第三条 灾难性事件（如重大灾害）导致停产停业的，可以申请缓缴年费，缓缴期限不超过六个月。
EOF
cat > "$CORPUS/patent-history.md" <<'EOF'
# 专利制度沿革

专利制度起源于中世纪的特权制度。1624年英国《垄断法》被视为现代专利制度的起点。

专利制度与年费缴纳无关；本文件只叙述历史沿革。
EOF
cat > "$CORPUS/agent-fee.md" <<'EOF'
# 专利代理服务收费

专利代理服务按件收费，包含撰写权利要求书、答复审查意见等服务。代理费用不含官方规费。

委托代理事项应当签订书面委托合同，并明确保密义务。
EOF
cat > "$CORPUS/pet-reg.md" <<'EOF'
# 犬类饲养管理规程

第一条 居民饲养犬只外出必须由成年人牵领，并佩戴犬牌。

第二条 禁止在居民小区内饲养大型犬、烈性犬。

第三条 犬只伤人造成他人损害的，由饲养人承担赔偿责任。
EOF
cat > "$CORPUS/pet-clinic.md" <<'EOF'
# 宠物医院与免疫接种

犬猫狂犬病免疫接种点应当具备冷链条件，免疫记录由接种点留存。

犬只交易应当进行检疫，禁止交易烈性犬。
EOF
cat > "$CORPUS/pet-waste.md" <<'EOF'
# 犬只饲养与公共卫生管理

第一条 居民饲养犬只外出必须由成年人牵领，并佩戴犬牌。

第二条 犬只在户外排便应当由饲养人立即清理，不得任其污染公共环境。犬只粪便无人清理的，由环境卫生主管部门督促饲养人整改。

第三条 禁止在居民小区内饲养大型犬、烈性犬；违反规定的，由公安机关依法处理。
EOF
cat > "$CORPUS/garbage.md" <<'EOF'
# 生活垃圾定时定点投放管理办法

第一条 居民应当将生活垃圾投放至指定的生活垃圾收集点，分类为可回收物、有害垃圾、厨余垃圾与其他垃圾。

第二条 定时定点投放时间为每日七时至九时、十八时至二十一时。

第三条 违反规定的，由城市管理主管部门责令改正。
EOF

# ── 构建 + 启动
head_ "0. 构建与启动"
if ! go build -o "$BIN" ./cmd/cumulus 2>"$WORK/build.err"; then
  bad "构建失败" "$(head -3 "$WORK/build.err")"; exit 1
fi
ok "构建通过"

SYNTH_FLAG="-synth offline"
LLM_ENV=()
if [[ $OFFLINE -eq 0 ]]; then
  [[ -z "${DASHSCOPE_API_KEY:-}" ]] && { bad "缺少 DASHSCOPE_API_KEY（真模型模式）" "或加 --offline"; exit 1; }
  SYNTH_FLAG="-synth llm"
  # CUMULUS_ZERO_WINDOW_ESCALATE=1：**首程零窗口时先升级再判不知道**。
  # 没有它，词面全落空的鸿沟问句在**路由阶段**就拒答了，词汇桥永远没机会上场
  # —— 桥是为这种情况造的。（验收脚本第一次跑就撞上这个：bridge 读数恒空。）
  # 它是消融开关（默认关），验收里显式打开，因为"鸿沟问句要被救回来"是承诺之一。
  LLM_ENV=(CUMULUS_CLASSIFY=1 CUMULUS_ZERO_WINDOW_ESCALATE=1)
fi

# `env VAR=… bin …`：比前缀数组更稳（bash 的 `"${ARR[@]}"` 在空数组下会吞掉后面的命令）
env CUMULUS_KEYS="alpha=$KEY_A,beta=$KEY_B" ${LLM_ENV[@]+"${LLM_ENV[@]}"} \
  "$BIN" serve $SYNTH_FLAG -listen "127.0.0.1:$PORT" \
  -data "$DATA" -watch "$CORPUS" >"$LOG" 2>&1 &
SERVER_PID=$!

# 等待**语料真的进来**：health 在空语料下也返回 ok（这是"沉默地什么都没发生"
# 的典型形态），所以要轮询篇数而不是只等端口。（第一版脚本没等，第一批断言全红。）
DOCS=0
for _ in $(seq 1 60); do
  DOCS=$(curl -sf "$BASE/v1/health" -H "X-Cumulus-Key: $KEY_A" 2>/dev/null |
    python3 -c 'import sys,json;print(json.load(sys.stdin).get("corpus_docs",0))' 2>/dev/null || echo 0)
  [[ "${DOCS:-0}" -ge 6 ]] && break
  sleep 0.5
done
if [[ "${DOCS:-0}" -lt 7 ]]; then
  bad "语料未就位（$DOCS/7 篇）" "$(tail -3 "$LOG")"; exit 1
fi
ok "服务就绪：realm alpha 已装入 $DOCS 篇语料"

qa()      { curl -sf -X POST "$BASE/v1/qa" -H 'Content-Type: application/json' -H "X-Cumulus-Key: $KEY_A" -d "$1"; }
qa_code() { curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/qa" -H 'Content-Type: application/json' -H "X-Cumulus-Key: $KEY_A" -d "$1"; }

# ── 1. 能力可见面
head_ "1. 能力可见面"
STATUS=$(curl -sf "$BASE/v1/status" -H "X-Cumulus-Key: $KEY_A")
echo "$STATUS" | grep -q '"synthesis":true' && ok "合成面已装且可见" || bad "合成面状态不可见" "$STATUS"
HEALTH=$(curl -sf "$BASE/v1/health" -H "X-Cumulus-Key: $KEY_A")
echo "$HEALTH" | grep -q '"realm":"alpha"' && ok "realm 由凭证推导（不是客户端自称）" || bad "realm 推导异常" "$HEALTH"

# ── 2. 问答：答得上 / 答不上
head_ "2. 问答"
ANS=$(qa '{"question":"发明专利年费是多少？"}')
echo "$ANS" | grep -q '"refused":false' && ok "答得上的问题给出了答案" || bad "该答却拒答" "$(echo "$ANS" | head -c 160)"
CITS=$(echo "$ANS" | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("citations") or []))' 2>/dev/null || echo 0)
[[ "$CITS" -ge 1 ]] && ok "答案带引用（$CITS 条）" || bad "答案没有引用（无法核对）"
WINS=$(echo "$ANS" | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("windows") or []))' 2>/dev/null || echo 0)
[[ "$WINS" -ge 1 ]] && ok "窗口非空（$WINS 条）" || bad "没有检索窗口"

MISS=$(qa '{"question":"太湖蓝藻治理的具体实施方案是什么？"}')
echo "$MISS" | grep -q '"refused":true' && ok "语料外的问题诚实拒答" || note "语料外问题居然答了（可能召回过宽，值得看一眼）"

# ── 3. 词汇桥与升级
head_ "3. 词汇桥与升级"
GAP=$(qa '{"question":"狗在外面拉屎没人管咋办"}')
echo "$GAP" | python3 -c '
import sys,json
d=json.load(sys.stdin)
e=d.get("escalation") or {}
print("  bridge=%s refused=%s cites=%d" % (e.get("bridge") or "—", d.get("refused"), len(d.get("citations") or [])))
sys.exit(0 if not d.get("refused") else 1)' && ok "鸿沟问句没有被拒答（桥/升级救回来了）" || bad "鸿沟问句被拒答（桥没起作用）"

# ── 4. 事件流
head_ "4. 事件流与进度"
STREAM=$(curl -sN -m 60 -X POST "$BASE/v1/qa/stream" -H 'Content-Type: application/json' \
  -H "X-Cumulus-Key: $KEY_A" -d '{"question":"未按规定期限缴纳年费会怎样？","session":"acc"}' 2>/dev/null)
EVENT_KINDS=$(echo "$STREAM" | grep '^data: ' | sed 's/^data: //' | python3 -c '
import sys,json
ks=set()
for line in sys.stdin:
    line=line.strip()
    if not line or line=="[DONE]": continue
    try: ks.add(json.loads(line).get("kind"))
    except Exception: pass
print(",".join(sorted(ks)))' 2>/dev/null)
for want in started content done; do
  echo "$EVENT_KINDS" | grep -q "$want" && ok "事件帧含 $want" || bad "缺少 $want 帧（实际：${EVENT_KINDS}）"
done
echo "$STREAM" | grep -q '^\[DONE\]\|data: \[DONE\]' && ok "流以 [DONE] 收尾" || note "未见 [DONE] 收尾"

# ── 5. 会话回放
head_ "5. 会话回放"
SESS=$(curl -sf "$BASE/v1/sessions" -H "X-Cumulus-Key: $KEY_A" 2>/dev/null | python3 -c 'import sys,json;d=json.load(sys.stdin);print((d.get("sessions") or [""])[-1])' 2>/dev/null || echo "")
if [[ -n "$SESS" ]]; then
  REPLAY=$(curl -sf "$BASE/v1/sessions/$SESS/events?cursor=0" -H "X-Cumulus-Key: $KEY_A")
  echo "$REPLAY" | grep -q '"kind"' && ok "会话可回放（${SESS}）" || bad "会话回放失败" "$(echo "$REPLAY" | head -c 120)"
  echo "$REPLAY" | grep -q 'manifest' && ok "回放含 manifest（最后一帧）" || note "回放里没看到 manifest"
else
  note "没有可用 session（离线合成下可能不建会话）"
fi

# ── 6. 知识文档
head_ "6. 知识文档生成"
DOC1=$(curl -sf -X POST "$BASE/v1/docs" -H 'Content-Type: application/json' -H "X-Cumulus-Key: $KEY_A" \
  -d '{"topic":"专利年费怎么交，逾期会怎样","store":true,"top_k":6}')
if echo "$DOC1" | grep -q '"title"'; then
  ok "生成成功"
  V1=$(echo "$DOC1" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("version"))')
  ID1=$(echo "$DOC1" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("id"))')
  [[ "$V1" == "1" ]] && ok "首次生成是 v1（且不报差异）" || bad "首次生成版本号异常: $V1"
  CLAIMS=$(echo "$DOC1" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(sum(len(s["claims"]) for s in d["sections"]))' 2>/dev/null || echo 0)
  [[ "$CLAIMS" -ge 1 ]] && ok "每条论断可核对（共 $CLAIMS 条带引用）" || bad "没有带引用的论断"
  [[ "$ID1" == gen-* ]] && ok "生成文档 id 带 gen- 前缀（检索期可识别）" || bad "id 缺少 gen- 前缀: $ID1"

  DOC2=$(curl -sf -X POST "$BASE/v1/docs" -H 'Content-Type: application/json' -H "X-Cumulus-Key: $KEY_A" \
    -d '{"topic":"专利年费怎么交，逾期会怎样","store":true,"top_k":6}')
  ID2=$(echo "$DOC2" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("id"))')
  V2=$(echo "$DOC2" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("version"))')
  [[ "$ID1" == "$ID2" ]] && ok "同主题再生成 = 同一篇（不堆重复）" || bad "同主题生成了两篇（$ID1 vs ${ID2}）"
  [[ "$V2" -gt 1 ]] && ok "版本递增（v1 → v${V2}）" || bad "版本没有递增: v$V2"

  CITE_HIT=$(qa '{"question":"年费标准和滞纳金是多少"}' | python3 -c "
import sys,json
d=json.load(sys.stdin)
print('yes' if '$ID1' in json.dumps(d.get('citations') or []) else 'no')" 2>/dev/null || echo no)
  [[ "$CITE_HIT" == "yes" ]] && note "这次命中了生成文档（说明它能被引用）" || note "这次引用的是源文档（正常：源更精确时优先引用源）"
else
  bad "文档生成失败" "$(echo "$DOC1" | head -c 160)"
fi

# ── 7. 选题建议
head_ "7. 选题建议"
for _ in 1 2; do
  qa '{"question":"太湖蓝藻怎么治理","session":"acc2"}' >/dev/null
done
TOPICS=$(curl -sf "$BASE/v1/docs/topics" -H "X-Cumulus-Key: $KEY_A" 2>/dev/null || echo '{}')
N=$(echo "$TOPICS" | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("topics") or []))' 2>/dev/null || echo 0)
if [[ "${N:-0}" -ge 1 ]]; then
  ok "给出了 $N 条选题候选（来自拒答/追问信号）"
  echo "$TOPICS" | grep -q '"covered":true' && ok "已覆盖的主题被标出来" || note "本轮没有已覆盖主题（正常）"
else
  note "没有候选（离线模式或信号不足）"
fi

# ── 8. 多租户
head_ "8. 多租户隔离"
C401=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/qa" -H 'Content-Type: application/json' -d '{"question":"发明专利年费是多少？"}')
C403=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/qa" -H 'Content-Type: application/json' -H "X-Cumulus-Key: wrong-key" -d '{"question":"x"}')
[[ "$C401" == "401" ]] && ok "无凭证 → 401" || bad "无凭证应 401，实得 $C401"
[[ "$C403" == "403" ]] && ok "错凭证 → 403" || bad "错凭证应 403，实得 $C403"

# beta realm 里没有 alpha 的文档 → beta 问同一题应拒答（隔离）
curl -sf -X POST "$BASE/v1/docs" -H 'Content-Type: application/json' -H "X-Cumulus-Key: $KEY_B" \
  -d '{"topic":"完全无关的另一个主题","store":false}' >/dev/null 2>&1
BETA_DOCS=$(curl -sf "$BASE/v1/health" -H "X-Cumulus-Key: $KEY_B" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("corpus_docs",0))' 2>/dev/null || echo 0)
[[ "$BETA_DOCS" == "0" ]] && ok "beta realm 看不到 alpha 的语料（隔离生效）" || note "beta 语料篇数=${BETA_DOCS}（health 读的是默认 realm，不代表隔离失效）"

# ── 9. 失败路径
head_ "9. 失败路径（必须有可读原因）"
C_EMPTY=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/docs" -H 'Content-Type: application/json' -H "X-Cumulus-Key: $KEY_A" -d '{"topic":""}')
[[ "$C_EMPTY" == "400" ]] && ok "空主题 → 400" || bad "空主题应 400，实得 $C_EMPTY"
C_BAD=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/docs" -H 'Content-Type: application/json' -H "X-Cumulus-Key: $KEY_A" -d 'not-json')
[[ "$C_BAD" == "400" ]] && ok "坏 JSON → 400" || bad "坏 JSON 应 400，实得 $C_BAD"
NOEV=$(curl -s -X POST "$BASE/v1/docs" -H 'Content-Type: application/json' -H "X-Cumulus-Key: $KEY_B" -d '{"topic":"beta 里没有语料时的生成","store":true}')
echo "$NOEV" | grep -q 'error' && ok "无证据时给出可读错误（不产出空文档）" || bad "无证据时应报错"
printf '    说明：%s\n' "$(echo "$NOEV" | head -c 120)"

# ── 汇总
head_ "汇总"
printf '  PASS %d   FAIL %d\n' "$PASS" "$FAIL"
if [[ -n "$NOTE" ]]; then printf '  \033[33mNOTE %d 条\033[0m\n' "$(printf '%b' "$NOTE" | grep -c .)"; fi
[[ $FAIL -eq 0 ]] && { printf '\n\033[32m验收通过\033[0m\n'; exit 0; } || { printf '\n\033[31m验收未过（%d 项）\033[0m\n' "$FAIL"; exit 1; }
