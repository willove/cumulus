# internal/minilm — vendored 件

上游：cumulus 仓库 `internal/minilm`（同作者，2026-10-07 拷贝自 cumulus master
的对应目录）。模型：`sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2`
（ModelScope 下载，约 485MB，384 维，12 层 12 头）。

## 为什么 vendor 而不是引依赖

- 纯 Go 自实现（safetensors 解析 + transformer forward + tokenizer + 归一化表），
  零外部依赖，cumulus 侧也没有独立发布；
- 参照 deepseek-harness vendor cordis 的纪律：**框架层自持**（可审计、可改、
  版本钉死）。

## 已做的本地修改

1. 环境变量前缀 `CLUS_*` → `CUMULUS_*`：
   `CUMULUS_MINILM_REQUIRE` / `CUMULUS_MODEL_DIR` / `CUMULUS_MINILM_DIR` /
   `CUMULUS_EMBED`；
2. 安装命令提示 `cumulus-cluster model install` → `cumulus model install`；
3. 未动：前向计算、分词、权重布局、manifest 格式。

## 更新方式

cumulus 侧该目录有更新时：整目录比对同步，重放或退役上面的本地修改，
跑 `go test ./internal/...` 后再提交。**不要在本目录里演进算法**——要改先
改 cumulus 上游，再同步。

## 权重

权重不进 git（`~/.cumulus/models/<ModelID>` 或 `$CUMULUS_MODEL_DIR`）。
安装：`cumulus model install`（ModelScope + sha256 清单校验）。权重缺席时
`Available()` 为假——语义组件因此不激活（见 `internal/qaflow/rerank.go`
的 SemanticRerank），BM25 序原样保留。
