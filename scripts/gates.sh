#!/usr/bin/env bash
# 门禁：零容错。任何一门红，整体红。
set -euo pipefail
cd "$(dirname "$0")/.."

FAIL=0
gate() { printf '[gate] %-28s' "$1"; shift; if "$@"; then echo "ok"; else echo "FAIL"; FAIL=1; fi; }

gate "gofmt" bash -c 'test -z "$(gofmt -l .)"'
gate "vet" go vet ./...
gate "test" go test ./...
gate "grammar-conformance" go test ./internal/qaflow/ -run 'TestConfinement|TestUnwind|TestRegister|TestEvidenceVerify|TestRouteRefuses' -count=1

# 待补：
#   contract-gen   Go 结构体 → OpenAPI + TS 类型，产物过期即红（修 conf/confidence 那类漂移）
#   doc-fresh      docs 引用的符号路径存在；ADR 编号连续
#   file-size      单文件行数上限（cumulus 的 deep.go 2212 行不再发生）

if [ "$FAIL" -eq 0 ]; then echo "all gates green"; else echo "GATES RED"; exit 1; fi
