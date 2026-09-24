SHELL := /bin/bash

.PHONY: all fmt check test build e2e clean help

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
	go build -o bin/ask ./cmd/ask

## e2e: 门 A–T——真 cumulite 嵌入式库（无服务端进程）
e2e:
	bash scripts/e2e.sh

## clean: 清理构建产物
clean:
	rm -rf bin/

## help: 列出目标
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
