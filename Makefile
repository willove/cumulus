SHELL := /bin/bash

.PHONY: all fmt check test build e2e browser-check browser-live score-probe web web-test clean help

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
## ⚠ 2026-10-03 起此门为**陈旧**（退出码 3），不是环境缺席也不是新回归：两个 .mjs
##   断言的是 b8a17e8（2026-09-25）那一代评测工作台，7181b66（2026-09-29，4 导航 IA）
##   把它整个换掉了——37 个断言名里 27 个在 web/src 已不存在。重写排在
##   docs/ui-v4-design.md §三 W5（等 IA 定形，免得重写两遍）。
##   make 对任何配方失败都退 2，与脚本自己的「2=环境缺席」撞码，所以这里打出真实码。
browser-check:
	@bash scripts/browser-eval-check.sh; rc=$$?; \
	  echo "browser-check: 脚本退出码=$$rc（0=通过 1=真实失败 2=环境缺席 3=断言陈旧）；make 自身对任何失败一律退 2"; \
	  exit $$rc

## web: 重建内嵌 UI 产物并刷新新鲜度戳。web/dist 是**提交进 git 的构建产物**
## （webui.go 的 go:embed 要在编译期找到它，且运行时零 Node），所以改完 web/src
## 必须跑这个并把 dist 一起提交——否则二进制服务的还是旧 UI，而两者输出长得一样。
web:
	cd web && npm run build
	WEB_STAMP_UPDATE=1 go test ./cmd/cumulus-cluster -run TestWebDistMatchesWebSrc -count=1

## web-test: 前端组合式函数测试（纯 JS，不起浏览器；渲染层归 browser-check）
web-test:
	cd web && npm test

## browser-live: 真实模型付费面检查——只读一次已完成的 live 运行，不提交、不产生费用
## 两个前提（缺任一退 2，不是退 1）：serve 要带 LLM_BASE_URL（live_available 只看配置，
## 门不提交所以随便指一个假端点也不花钱），且那个 serve 的二进制要在 `make web` 之后重新
## build 过——包是编译期嵌进去的，旧二进制会让所有断言对着已经不存在的 UI 判绿。
browser-live:
	EVAL_BASE="$(EVAL_BASE)" EVAL_NS="$(EVAL_NS)" EVAL_SHOTS="$(EVAL_SHOTS)" node scripts/browser/eval-live-surfaces.mjs

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
