package qaflow

import "github.com/willove/cumulus/internal/context"

// 本文件是分类器纪律的注释档案。
//
// BeliefBooster（第一个 Activator 实例）已随全局声望版 belief 退役——
// 真实语料三臂 A/B 证实它有害（−11pp，51 丢 / 18 赚，见 evolution-log
// 三·补七）。它执行的事由 SemanticRerank（rerank.go）接替：
// 同样只要求一件事（embedder 绑着），绑了激活、撤了停用、绝不静默
// 失效——cumulus 的 MCS 静默不触发事故，根因就是没有这个分类器。
//
// 纪律原文保留：可选组件的启用/停用必须可见；组件不实现策略逻辑本身，
// 只声明依赖与生死。
var _ = context.NewKey[int] // 占位防误删整包导入，运行时不产生引用
