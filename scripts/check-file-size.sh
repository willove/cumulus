#!/usr/bin/env bash
# file-size 门：单文件行数上限。cumulus 的 deep.go 长到 2212 行——
# 改动相互踩、review 不可能、测试覆盖不到，那个文件是教训本身。
# 上限 600：当前最大 531（qa.go），留 13% 生长余量，超了就是该拆的信号。
set -euo pipefail
cd "$(dirname "$0")/.."

LIMIT=600
fail=0
while IFS= read -r line; do
  n=$(echo "$line" | awk '{print $1}')
  f=$(echo "$line" | awk '{print $2}')
  if [ "$n" -gt "$LIMIT" ]; then
    echo "  $f: $n lines > $LIMIT — 拆它（按职责拆，不是按行数硬切）"
    fail=1
  fi
done < <(find internal cmd -name '*.go' -not -name '*_test.go' | xargs wc -l | grep -v ' total$')

if [ "$fail" -eq 1 ]; then exit 1; fi
biggest=$(find internal cmd -name '*.go' -not -name '*_test.go' | xargs wc -l | grep -v ' total$' | sort -rn | head -1)
echo "biggest: $biggest (limit $LIMIT)"
