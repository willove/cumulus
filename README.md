# cumulus-next

对持续增长的本地语料做自然语言检索与问答：证据可核、策略可回滚、系统越用越准。

cumulus 的下一版。区别不在功能，在结构：一切共享状态经过一个 context、一切修改可逆、一切依赖显式、每次处理留提交视图。

## 读文档的顺序

1. [`docs/architecture.md`](docs/architecture.md)——架构总图 + 从 cumulus 继承的教训（问题→对策→执行处）
2. [`docs/flow-grammar.md`](docs/flow-grammar.md)——流程文法 SSOT：三个流程的 stage 契约、六条不变量、失败六分类
3. [`docs/evolution-log.md`](docs/evolution-log.md)——演进记录：门禁抓到过什么、立下了哪些规矩、还没验证什么
4. [`docs/web-audit.md`](docs/web-audit.md)——web 重建前的审计（内容去留与重建纪律）
5. [`docs/adr/`](docs/adr/)——决策记录（含 ADR-001：为什么不做代码级热替换）

## 现状（v0.1 骨架）

已落地并能跑：

```
internal/context/   typed key、realm、注册/释放、提交视图、stage 禁闭
internal/flow/      stage 契约 + runner（禁闭、失败反卷、记视图）
internal/qaflow/    问答六 stage（检索/路由/验证是真的，合成为桩；信念可绑可选）
internal/evalfcore/ 评测 episode：三指纹冻结、原子落盘、中断不重跑、失败六分类
internal/learncore/ 受管变更五阶段：旋钮白名单、率地板护栏、提升=注册、掉线回滚
internal/knowledge/belief/ 候选区后验（observe-update 的 update 半边，纯函数）
internal/retrieval/ 倒排索引 + BM25 + CJK 二元组分词 + 证据窗口坐标（cumulus 同款）
internal/failure/   失败模式六分类
internal/store/     存储端口 + cumulite 适配器（线上版 github.com/willove/cumulite v0.2.1）
cmd/cumulus/        selftest 命令
```

```bash
scripts/gates.sh                    # 门禁（零容错）
go run ./cmd/cumulus-next selftest  # 跑一次骨架问答，打印提交视图
```

## 下一步（按序）

1. ~~BM25 检索接进 `qaflow` 的 evidence stage~~（已落地：`internal/retrieval`，selftest 可见窗口坐标与原文还原）
2. ~~`evalfcore`：评测 episode~~（已落地：`cumulus eval` 可见三指纹/逐题归因/摘要；判官臂 N/A≠0、成本未知显式计数）
3. ~~`learncore`：受管变更五阶段~~（已落地：`cumulus learn` 可见 diagnose→propose→evaluate→promote 全链路，含召回不足→topk 提升的真案例）
4. evalfcore 判官臂接 LLM（Judge 接口已在，判官为空即 N/A）
5. ~~belief 接进候选排序~~（已接线：`cumulus learn` 末尾可见“只留信念、topk 调回 3 仍 100%”）
4. store 端口补 KV 与查询（现在只有文档 CRUD，按流程需求长方法）+ retrieval 从 store 装语料
5. HTTP 面 + 契约生成（Go 结构体 → OpenAPI + TS 类型）
6. 反应式依赖分类器（可选组件激活/停用/中性的运行时判定——cumulus 老病灶，下一轮）
7. `web/` 按 `docs/web-audit.md` 重建，第一个页面是“问答”

## 已定的决定（2026-10-07）

- 模块名 `github.com/willove/cumulus`，分支策略：本目录先走 next 分支，稳定后逐步切 main；
- 存储用线上 cumulite（`github.com/willove/cumulite` v0.2.1），经 `internal/store` 端口接入，业务代码不直接依赖它；
- web 维持 Vite + Vue 系，内容按 [`docs/web-audit.md`](docs/web-audit.md) 逐块审计后重建，契约先于页面。
