// Package arch 是**项目分层与依赖方向的机械保证**。
//
// 为什么需要它：internal 下有二十几个包，此前**没有任何地方声明过"哪个包属于
// 哪一层、谁可以依赖谁"**。依赖方向靠人记着——而人记不住。已发生过的漂移：
// evalfcore 反向依赖 context、synth 依赖 qaflow、judge 依赖 evalfcore。
//
// 这个包把分层写成数据、把方向写成规则，规则由测试对着**真实导入图**跑一遍
// （TestLayerDirectionHolds）。违反即红——与 grammar-conformance 同一套路：
// 不靠评审靠门禁。
//
// 分层（按当前真实依赖图划定，不是一张理想蓝图）：
//
//	kernel       底座原语。只能依赖 kernel。不许依赖任何上层。
//	ports        外部世界端口（存储/决策面）。只能依赖 kernel。
//	capabilities 能力件（检索、合成、判断、评测、学习……）。可依赖
//	             kernel/ports/capabilities；**不许依赖 pipeline/apps**。
//	pipeline     流程编排（qaflow 及其合成/判官面）。可依赖下面三层；
//	             **不许依赖 apps**。
//	apps         应用面（api、cmd）。可依赖任何层，但不许被下层依赖。
//
// 被这些规则**禁止**的（也就是它们防住的事）：底座被业务流程反向依赖、
// 能力件长出对流程的依赖（那样它就不能单独复用）、流程去依赖应用面。
package arch

import (
	"fmt"
	"sort"
	"strings"
)

// Layer 是分层。
// LayerTooling 是**开发自用工具**（评测、校准、学习闭环、人工批注）。
//
// 它与 capabilities 的区别不是"重要性"，而是**谁需要它**：
//   - capabilities 是**引擎运行**要的能力（检索、合成、判官……）；
//   - tooling 只在"我们开发/测量这个引擎"时才需要（跑题集、看读数、改阈值）。
//
// 分层的意义是一条可执行的边界：**引擎核心（kernel/ports/capabilities/pipeline/host）
// 不得依赖 tooling**。没有这条线，"为了加个读数就 import 评测包"会在几个月里
// 把引擎拖成它自己��工具。
//
// 2026-10 起 tooling 整体**暂停投入**（实测收益弱于把知识文档做扎实）——
// 代码保留（不删已有工作），但不再新增。
type Layer int

// 分层取值。
const (
	LayerKernel Layer = iota
	LayerPorts
	LayerCapabilities
	LayerPipeline
	LayerApps
	// LayerTooling 排在最后：**它被允许依赖任何层**（评测要用引擎的一切），
	// 但任何层**不得依赖它**（引擎运行不需要工具）。
	LayerTooling
)

// String 层名（读数/报错用）。
func (l Layer) String() string {
	switch l {
	case LayerKernel:
		return "kernel"
	case LayerPorts:
		return "ports"
	case LayerCapabilities:
		return "capabilities"
	case LayerPipeline:
		return "pipeline"
	case LayerApps:
		return "apps"
	case LayerTooling:
		return "tooling"
	default:
		return "unknown"
	}
}

// packages 是包 → 层。**新增包必须在这里登记**——测试会检查"有包没登记"
// 并让它红：新包不声明归属，就等于没约束。
var packages = map[string]Layer{
	// kernel：底座原语（typed key / stage runner / 回包解析）
	"context": LayerKernel,
	"flow":    LayerKernel,
	"marks":   LayerKernel,
	// arch 是**构建期门禁**（读导入图、判方向），运行期不参与流程；
	// 归 kernel 是因为它只做纯分析、不依赖任何上层。
	"arch": LayerKernel,
	// ports：外部世界端口
	"store":  LayerPorts,
	"decide": LayerPorts,
	// capabilities：能力件
	"auth":    LayerCapabilities, // 凭证 → realm（鉴权网关；不依赖任何上层）
	"abstain": LayerCapabilities, // 弃答权重门（运行时能力：facts 阶段在用）
	"calib":   LayerTooling,
	"corpus":  LayerCapabilities,
	"ctxmgmt": LayerCapabilities,
	// docgen 是**知识文档生成**（语料的生产入口）：依赖检索与 LLM，
	// 被 api 调用。它是 capabilities（运行时能力，不是 tooling）。
	"docgen": LayerCapabilities,
	// usage 是**用量计量与配额**（运行时能力：共享实例的必需品）
	"usage":             LayerCapabilities,
	"deepcore":          LayerCapabilities,
	"embed":             LayerCapabilities,
	"evaldata":          LayerTooling,
	"evalfcore":         LayerTooling,
	"facts":             LayerCapabilities,
	"harness":           LayerCapabilities, // 对外输出面（只依赖 kernel；依赖 pipeline 会被 boundary 拦住）
	"harness/evokechat": LayerCapabilities, // 协议适配（翻成 invoke-chat 调用序列；零依赖，不认识 UI）
	"failure":           LayerCapabilities,
	"ingest":            LayerCapabilities,
	"knowledge":         LayerCapabilities, // 复用命中 + 使用信号（运行时能力，不是评测工具）
	"knowledge/belief":  LayerCapabilities,
	"learncore":         LayerTooling,
	"llm":               LayerCapabilities,
	"minilm":            LayerCapabilities,
	"prior":             LayerCapabilities,
	"query":             LayerCapabilities,
	"rerank":            LayerCapabilities,
	"retrieval":         LayerCapabilities,
	// pipeline：流程编排
	"judge":  LayerTooling, // 只有评测/verify 用（cmd/cumulus），引擎不依赖
	"qaflow": LayerPipeline,
	"synth":  LayerPipeline,
	// apps：应用面（api 与 cmd/** 都属应用层）
	"api":              LayerApps,
	"cmd/cumulus":      LayerApps,
	"cmd/contract-gen": LayerApps,
	"cmd/doc-fresh":    LayerApps,
	"cmd/humanbatch":   LayerApps,
}

// layersOrdered 是"可以依赖"的顺序：下标小的可以被下标大的依赖。
var layersOrdered = []Layer{LayerKernel, LayerPorts, LayerCapabilities, LayerPipeline, LayerApps}

// Dependency 是一条内部依赖边（from → to，都是相对 module 的路径）。
type Dependency struct{ From, To string }

// Violation 是一条违反方向的依赖。
type Violation struct {
	From, To           string
	FromLayer, ToLayer Layer
	Reason             string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s(%s) → %s(%s)：%s", v.From, v.FromLayer, v.To, v.ToLayer, v.Reason)
}

// Check 对一组内部依赖跑分层规则，返回违反项（空 = 合规）。
//
// 同层依赖是允许的（apps 之间如 cmd → api；capabilities 之间的能力件互依）——
// 我们约束的是**方向**，不是"谁不能跟谁讲话"。
func Check(deps []Dependency) []Violation {
	var out []Violation
	for _, d := range deps {
		fl, ok := LayerOf(d.From)
		if !ok {
			out = append(out, Violation{d.From, d.To, Layer(-1), Layer(-1), "源包未在分层表里登记（新增包必须登记）"})
			continue
		}
		tl, ok := LayerOf(d.To)
		if !ok {
			out = append(out, Violation{d.From, d.To, fl, Layer(-1), "目标包未在分层表里登记"})
			continue
		}
		if fl == tl {
			continue // 同层：允许
		}
		// tooling 是**正交的**，不走方向链：它被允许依赖任何层（评测要用引擎的一切），
		// 而任何层**不得依赖它**（引擎运行不需要工具）。
		if tl == LayerTooling {
			// 允许"工具子命令"与"引擎面上明确挂着的读数接口"，其余一律违规——
			// 这条例外必须显式列出：隐式放行等于没有规则。
			if allowedToolingUse(d.From, d.To) {
				continue
			}
			out = append(out, Violation{d.From, d.To, fl, tl,
				fmt.Sprintf("%s 不得依赖 tooling 包 %s（引擎不需要工具；要工具的只有开发面）", d.From, d.To)})
			continue
		}
		if rank(fl) > rank(tl) {
			continue // 下层被上层依赖：正是要的
		}
		out = append(out, Violation{d.From, d.To, fl, tl,
			fmt.Sprintf("%s 层不许依赖 %s 层（方向反了）", fl, tl)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// allowedToolingUse 是"依赖 tooling"的**全部合法例外**（逐条带理由，新增要写理由）。
//
// 规则的价值在于例外是**显式且有界的**：允许"cmd/cumulus 的 eval/calib/learn 子命令"
// 与"api 的使用信号读数"，其余任何包碰 tooling 都红。
func allowedToolingUse(from, to string) bool {
	switch from {
	case "cmd/cumulus":
		// 工具的**宿主**：eval/calib/learn/verify 子命令就住在这儿。
		return true
	case "cmd/humanbatch":
		// 人工批注工具的宿主（评测面）：同 cmd/cumulus 的身份。
		return true
	case "api":
		// /v1/signals 露的是"线上使用信号的读数"——它是**服务能力**（运维看得到），
		// 不是评测功能。这里依赖 knowledge（信号库），learncore 只提供折算。
		return to == "knowledge" || to == "learncore"
	}
	return false
}

func rank(l Layer) int {
	for i, x := range layersOrdered {
		if x == l {
			return i
		}
	}
	return len(layersOrdered)
}

// prefixLayers 是"整类包"归属（新增 cmd/xxx 不必改表：它按前缀归层）。
var prefixLayers = map[string]Layer{
	"cmd/": LayerApps,
}

// LayerOf 查包所属层（精确表 → 前缀表）。
func LayerOf(pkg string) (Layer, bool) {
	if l, ok := packages[pkg]; ok {
		return l, true
	}
	for pre, l := range prefixLayers {
		if strings.HasPrefix(pkg, pre) {
			return l, true
		}
	}
	return 0, false
}

// Registered 返回已登记的包名（排序后）。
func Registered() []string {
	out := make([]string, 0, len(packages))
	for p := range packages {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// FormatViolations 把违反项排成一段可打印文本。
func FormatViolations(vs []Violation) string {
	if len(vs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, "  "+v.String())
	}
	return strings.Join(parts, "\n")
}
