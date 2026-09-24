#!/usr/bin/env bash
# R4 / §6.5 measurement — DIRECT LLM mode (debug/escape-hatch; production
# stays on the gateway for governance). Talks to the upstream itself:
#   AIGATE_BASE_URL=https://api.minimaxi.com/v1  AIGATE_CHAT_MODEL=MiniMax-M3
#
# Measures: FAST/DEEP/REUSE behavior on the real model (latency, confidence,
# resolved citations, reuse hit), plus a token-magnitude estimate sampled from
# ask's real prompt templates (MiniMax usage field). Recorded, not gated.
#
# Usage: bash scripts/endpoint-probe.sh [api_base]
#   key: AIGATE_API_KEY, or falls back to ~/.sirchmunk/.env (local dev only).
set -u
cd "$(dirname "$0")/.."
BASE="${1:-${AIGATE_BASE_URL:-https://api.minimaxi.com/v1}}"
MODEL="${AIGATE_CHAT_MODEL:-MiniMax-M3}"
if [ -z "${AIGATE_API_KEY:-}" ] && [ -f "$HOME/.sirchmunk/.env" ]; then
	# Local-dev fallback: the key the operator provisioned for sirchmunk.
	AIGATE_API_KEY="$(grep '^LLM_API_KEY=' "$HOME/.sirchmunk/.env" | cut -d= -f2- | sed -e 's/^"//' -e 's/"$//' -e "s/^'//" -e "s/'\$//")"
fi
[ -n "${AIGATE_API_KEY:-}" ] || { echo "endpoint-probe: AIGATE_API_KEY required"; exit 1; }

WORK="$(mktemp -d)"
STORE="$WORK/data"
trap 'rm -rf "$WORK"' EXIT

go build -o "$WORK/ask" ./cmd/ask || exit 1
"$WORK/ask" -data "$STORE" ensure >/dev/null

python3 - "$WORK" <<'PY'
import sys, os
w = sys.argv[1]
pad = "这片段是无关的填充内容，用于把文档撑过小文档阈值，进入采样路径。\n"
body = "# 部署手册\n\n## 连接池\n" + pad * 12 + "关键配置：连接池最大 128，超时 30 秒。\n" + pad * 12 + \
       "## 扩容\n单机连接数超过 3000 触发扩容评审，新上限由 capacity 模块计算。\n" + pad * 6
open(os.path.join(w, "manual.md"), "w").write(body)
PY
"$WORK/ask" -data "$STORE" put -title "部署手册" -key manual -body-file "$WORK/manual.md" >/dev/null

export AIGATE_BASE_URL="$BASE" AIGATE_CHAT_MODEL="$MODEL" AIGATE_API_KEY
A=("$WORK/ask" -data "$STORE")
echo "direct endpoint: $BASE model=$MODEL"

run() { # label query
	local S E
	S=$(date +%s.%N)
	"${A[@]}" search -q "$2" -raw >"$WORK/$1.json" 2>"$WORK/$1.err"
	E=$(date +%s.%N)
	if [ -s "$WORK/$1.err" ]; then
		printf '%s\n' "--- $1 stderr: $(head -c 200 "$WORK/$1.err")"
	fi
	python3 - "$WORK/$1.json" "$1" "$(echo "$E - $S" | bc)" <<'PY'
import json, sys
try:
    r = json.load(open(sys.argv[1]))
except Exception:
    print("%-6s FAILED to parse output" % sys.argv[2]); sys.exit()
a = r["answer"]; lat = float(sys.argv[3])
cits = (r.get("citations") or {}).get("refs") or []
print("%-6s mode=%-14s conf=%.3f calls=%d skipped=%s sampled=%2d escalated=%s latency=%5.1fs citations=%2d resolved=%2d"
      % (sys.argv[2], a["mode"], a["confidence"], a["llm_calls"], a["skipped"],
         len(a["samples"] or []), r.get("escalated"), lat, len(cits),
         sum(1 for c in cits if c.get("resolved"))))
PY
}

run FAST "连接池最大连接数是多少"
run REUSE "连接池最大连接数是多大"
run DEEP "北极狐栖息地的气候特征"

# Token magnitude: ask's real scorer prompt against the upstream usage field.
python3 - "$MODEL" <<'PY'
import json, os, urllib.request

base = os.environ["AIGATE_BASE_URL"].rstrip("/")
key = os.environ["AIGATE_API_KEY"]
model = os.environ["AIGATE_CHAT_MODEL"]

def usage_for(messages, tag):
    body = json.dumps({"model": model, "messages": messages, "temperature": 0, "max_tokens": 300}).encode()
    req = urllib.request.Request(base + "/chat/completions", data=body, headers={
        "Content-Type": "application/json", "Authorization": "Bearer " + key})
    with urllib.request.urlopen(req, timeout=180) as r:
        d = json.load(r)
    u = d.get("usage") or {}
    print("%-18s prompt=%s completion=%s total=%s" % (tag, u.get("prompt_tokens"),
          u.get("completion_tokens"), u.get("total_tokens")))

# The two shapes ask actually sends: evaluate_sample (per window) and
# synthesize_roi (once per answer), with 11 windows' worth of evidence.
win = "关键配置：连接池最大 128，超时 30 秒。" + "填充内容。" * 200
ev = "\n".join("[%d] (body [%d,%d)) %s" % (i + 1, i * 480, i * 480 + 480, win) for i in range(11))
usage_for([{"role": "user", "content":
    'User Query: "连接池最大连接数是多少"\n\nSample Source: manual\nSample Content:\n' + win}], "evaluate_sample×1")
usage_for([{"role": "user", "content":
    'Query: "连接池最大连接数是多少"\nEvidence:\n' + ev}], "synthesize_roi×1")
PY
echo "endpoint-probe done — direct mode, numbers are RECORD not gate"
