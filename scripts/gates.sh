#!/usr/bin/env bash
# 门禁：零容错。任何一门红，整体红。
set -euo pipefail
cd "$(dirname "$0")/.."

# go / gofmt 不一定在 PATH 里（后台任务与非交互 shell 的常见坑）。找不到就按
# SDK 常见位置找——门禁自己跑不起来时**整门红**，那比"没跑"更糟。
ensure_go() {
  command -v go >/dev/null 2>&1 && return 0
  for cand in /usr/local/go/bin /opt/homebrew/bin /usr/local/bin "$HOME/go/bin" "$HOME/sdk"/go*/bin; do
    if [[ -x "$cand/go" ]]; then export PATH="$cand:$PATH"; return 0; fi
  done
  return 1
}
ensure_go || { echo "gates: 找不到 go（装它或在 PATH 里放好）"; exit 2; }

FAIL=0
gate() { printf '[gate] %-28s' "$1"; shift; if "$@"; then echo "ok"; else echo "FAIL"; FAIL=1; fi; }

gate "gofmt" bash -c 'test -z "$(gofmt -l .)"'
gate "vet" go vet ./...
gate "test" go test ./...
# grammar-conformance：stage 契约（禁闭/反卷/注册）与**合流**（v0.2 §三.6：
# 随机装卸重放三断言——无悬挂/逆干净/终态等价）都是流程文法的执行处。
# boundary：分层与依赖方向（internal/arch）。新包不登记归属、或底层反向
# 依赖上层，这里红——依赖方向靠门禁，不靠评审记着。
gate "boundary" bash -c 'go test ./internal/arch/ -run "TestLayer|TestEvery|TestNoGhost" -count=1'
gate "grammar-conformance" go test ./internal/qaflow/ -run 'TestConfinement|TestUnwind|TestRegister|TestEvidenceVerify|TestRouteRefuses|TestConfluence' -count=1
# 哑对照臂不变量（v0.2 §三.7）：对照运行必须含最笨基线臂，缺了直接红。
gate "dumb-arm-invariant" go test ./internal/evalfcore/ -run 'TestValidateArmsRequiresDumb|TestRunArmsRejectsBeforeRunning' -count=1
gate "contract-gen" bash -c 'go run ./cmd/contract-gen -check'
gate "doc-fresh" go run ./cmd/doc-fresh
gate "file-size" bash scripts/check-file-size.sh


# 待补：

if [ "$FAIL" -eq 0 ]; then echo "all gates green"; else echo "GATES RED"; exit 1; fi
