SHELL := /bin/bash

.PHONY: all fmt check test build e2e browser-check browser-live score-probe clean help

all: check build

## fmt: gofmt -l 不过即失败
fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## check: fmt + vet + test（CI 口径）
check: fmt
	go vet ./...
	go test ./...

## test: 仅测试
test:
	go test ./...

## build: CLI 进 bin/
build:
	go build -o bin/cumulus-cluster ./cmd/cumulus-cluster

## e2e: 门 A–BB——真 cumulite 嵌入式库（无服务端进程）
e2e:
	bash scripts/e2e.sh

## browser-check: 浏览器联调门——自起离线 serve，打生产内嵌 /ui/（可选：需 node + @playwright/test）
browser-check:
	bash scripts/browser-eval-check.sh

## browser-live: 真实模型付费面检查——只读一次已完成的 live 运行，不提交、不产生费用
browser-live:
	EVAL_BASE="$(EVAL_BASE)" EVAL_NS="$(EVAL_NS)" node scripts/browser/eval-live-surfaces.mjs

## score-probe: 打分器可靠性探针（真实模型，只记录不设门）——同一窗口重复打分 N 次，
## 报告决策稳定性 / 阈值模糊带 / 金标分离度。P1-2 与 P1-3 的验收都靠它。
score-probe:
	go run ./cmd/scoreprobe \
	  -items "$(SCORE_ITEMS)" -corpus "$(SCORE_CORPUS)" \
	  -repeats "$(or $(REPEATS),5)" $(if $(THINKING_OFF),-thinking-off) \
	  -detail -json "$(or $(OUT),var/scoreprobe.json)"

## clean: 清理构建产物
clean:
	rm -rf bin/

## help: 列出目标
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
