package qaflow

import (
	"fmt"

	"github.com/willove/cumulus/internal/context"
)

// 路由判定（sufficiency-route stage）从 qa.go 拆出：路由是"要不要升级、
// 要不要拒答"的专职判定，输入族（覆盖/事实/冲突/置信）各自可观测。

func (RouteStage) Name() string { return "sufficiency-route" }
func (RouteStage) Reads() []string {
	return []string{KeyWindows.String(), KeyCoverage.String(), KeyDeep.String(), KeyFactReport.String(), KeyConflicts.String(), KeyDeepen.String()}
}
func (RouteStage) Writes() []string { return []string{KeyRoute.String()} }

// gammaStep 每多一条事实，升级线抬高的一格（cumulus B10 同值）：多事实
// 问句要 proportionally 更多的信心才许停在快路。上限相对基线（+3 格、
// 0.95 封顶）——绝对上限在抬高后的基线下会静默把线拉低。
const gammaStep = 0.05

// RouteConfig 是路由的阈值与校准来源（v0.2 §2.1：阈值不手调）。
//
// 两条来源都在这一个结构里，因为它必须进提交视图：换 provider 要重
// 校准、换校准程序要换版本标签，否则两次运行的数字不可比。
type RouteConfig struct {
	// UpgradeBase 是升级线基线 τ₀。<=0 用默认 0.5（手工值）。
	UpgradeBase float64
	// GammaStepOverride 覆盖每事实一格的步长；<=0 用默认 0.05。
	GammaStepOverride float64
	// Program 是阈值来源程序名：hand-set / CAUC。空 = hand-set。
	Program string
	// ThresholdVersion 是阈值版本标签（改程序或改校准集即改标签）。
	ThresholdVersion string
	// HasEscalate 说明**有升级执行处**（贵路/桥装配了）。零窗口时要不要先升级，
	// 取决于有没有地方可升——没有就只能是拒答（诚实的不知道）。
	//
	// 它是**接线时的声明**；运行期还要与 EscalateStage 的实际执行处一起判
	// （两个条件都满足才升）——配置说"有"但没装执行处时，那只是空转。
	HasEscalate bool
	// ZeroWindowEscalate：首程零窗口时**先升级再判不知道**（默认关）。
	//
	// 为什么这是开关而不是直接改：词面全落空时先拒答，等于**在唯一为这种情况
	// 造的机制（贵路 + 词汇桥）上场之前就放弃了**。但先升级也可能把"语料里真的
	// 没有"的情况变成"多花一次钱还是拒答"——两种都说得通，所以**先量再定**：
	// CUMULUS_ZERO_WINDOW_ESCALATE=1 开。CUMULUS_BRIDGE=0 时它仍然有用
	// （贵路本身就可能捞到词面失配的文档）。
	ZeroWindowEscalate bool
}

// ProgramName 归一化程序名（空值 = 手工）。
func (rc RouteConfig) ProgramName() string {
	if rc.Program == "" {
		return ProgramHandSet
	}
	return rc.Program
}

// ProgramHandSet / ProgramCAUC 是两个阈值来源程序。CAUC 的规则：
// 升级阈值 = 强臂（DEEP）在校准集上的经验准确率——低于这条线，弱臂
// 的可信度还不如直接花钱走强臂。程序名进提交视图，换程序必须换标签。
const (
	ProgramHandSet = "hand-set(v0.1)"
	ProgramCAUC    = "CAUC(tau0=deep-arm-accuracy)"
)

// Tau0FromDeepAccuracy 按 CAUC 从校准集读数推升级线：
// τ₀ = DEEP 臂在校准集上的经验准确率（钳到 [0,1]）。
//
// 边界（诚实写在这里，不许省）：CAUC 的 τ₀ 作用在**校准过的正确概率**
// 上；本项目的 Confidence 在 provider 无 logprobs 时是检索侧合成的
// 置信代理（档位 retrieval），不是正确概率。所以在跑出可靠性曲线
// （evalfcore.CalibrationTable）之前，τ₀ 只能当"待验证的候选值"，
// 默认仍是 hand-set 的 0.5——换程序要显式 opt-in（CUMULUS_TAU0）。
func Tau0FromDeepAccuracy(acc float64) float64 {
	if acc < 0 {
		return 0
	}
	if acc > 1 {
		return 1
	}
	return acc
}

// thresholdFor 按意图形状调升级线（fact 数量）。
func thresholdFor(base, step float64, factsK int) float64 {
	if step <= 0 {
		step = gammaStep
	}
	thr := base
	cap := thr + 3*step
	if cap > 0.95 {
		cap = 0.95
	}
	extra := factsK - 1
	if extra > 3 {
		extra = 3
	}
	thr += step * float64(extra)
	if thr > cap {
		thr = cap
	}
	return thr
}

func (s RouteStage) Run(c *context.Context) error {
	ws, _ := context.Get(c, KeyWindows)
	grounded := len(ws) > 0
	sig := gatherSignals(c, ws)
	sig.Tier = TierRetrieval
	sig.CalibrationProgram = s.Config.ProgramName()
	sig.ThresholdVersion = s.Config.ThresholdVersion

	d := RouteDecision{Grounded: grounded, Signals: sig}
	// 再问加深（KeyDeepen）：用户原样再问是比任何内部信号都硬的"上次
	// 不够"——本轮不走快路，强制升级（词汇桥+贵路重取）。这是用户说了
	// 算的开关，不是旋钮。
	if deepen, _ := context.Get(c, KeyDeepen); deepen {
		d.Action = "escalate"
		d.Reason = "re-ask in session: user says the last answer was not enough; escalating"
		if err := context.Set(c, KeyRoute, d); err != nil {
			return err
		}
		return nil
	}
	base := s.Config.UpgradeBase
	if base <= 0 {
		base = 0.5
	}
	threshold := thresholdFor(base, s.Config.GammaStepOverride, max(1, sig.FactsK))
	d.Signals.Threshold = threshold
	switch {
	case !grounded && s.Config.ZeroWindowEscalate && s.Config.HasEscalate && s.hasEscalate:
		// 词面全落空：**先升级**。贵路与词汇桥就是为这种情况造的（口语问句 vs
		// 书面文档），在它们上场之前就拒答 = 白造了。真跑发现：零窗口直接拒答
		// 让桥**一次都走不到**（三臂对照全是"未走桥 10 题"）。
		d.Action = "escalate"
		d.Reason = "no evidence window on fast path; escalate before refusing (词汇桥/贵路专为这种情况而设)"
	case !grounded:
		d.Action = "refuse" // 没有证据：诚实的不知道，不许硬答
		d.Reason = "no evidence window; refuse by grammar"
	case sig.Conflicts > 0:
		// 证据一致性门（cumulus：contested 的先验必须重搜，不许当普通
		// 答案服务）——同事实两窗给不同的数，升级让贵路再来
		d.Action = "escalate"
		d.Reason = fmt.Sprintf("%d evidence conflicts on the same fact (same fact, different values); re-search mandated",
			sig.Conflicts)
	case sig.FactsMissing > 0 && sig.FactsK > 1:
		// 多事实问句有事实缺口（cumulus：!Complete && len(fx)>1 才升级；
		// 单事实的缺口已被覆盖度信号管着，不重复升级）
		d.Action = "escalate"
		d.Reason = fmt.Sprintf("%d/%d facts uncovered (missing %d); escalate",
			sig.FactsMissing, sig.FactsK, sig.FactsMissing)
	case sig.Confidence < threshold:
		d.Action = "escalate"
		d.Reason = fmt.Sprintf("draft confidence %.3f below %.3f (coverage=%.3f margin=%.3f dead-rate=%.3f, facts %d/%d); escalate",
			sig.Confidence, threshold, sig.Coverage, sig.Margin, sig.DeadRate, sig.FactsCovered, sig.FactsK)
	default:
		d.Action = "fast"
		d.Reason = fmt.Sprintf("draft confidence %.3f (coverage=%.3f margin=%.3f, facts %d/%d)",
			sig.Confidence, sig.Coverage, sig.Margin, sig.FactsCovered, sig.FactsK)
	}
	return context.Set(c, KeyRoute, d)
}

// RouteSignals 是充足性路由的全部输入事实。全部来自流程内的可观测状态：
// 覆盖度（evidence.coverage）、区分度（窗口打分的头部分差）、死路率
// （deep 遥测）。没有调用方手填的数——手填置信度正是要修掉的旧形态。
type RouteSignals struct {
	Coverage   float64 `json:"coverage"`   // 查询词覆盖度（语料内可达词口径）
	Margin     float64 `json:"margin"`     // (top1-top2)/top1：候选区分度代理，0..1
	DeadRate   float64 `json:"dead_rate"`  // 死路/取样：翻过多少空文档
	Windows    int     `json:"windows"`    // 最终窗口数
	Confidence float64 `json:"confidence"` // 兜底档：上三项的加权组合（RetrievalConfidence）
	// Tier 是本次实际生效的信号档（v0.2 §2.1 三级）：logprob 优先 /
	// grounding 常备 / retrieval 兜底。**档位必须留痕**——把兜底档当
	// "草稿置信度"报出去，就是口径漂移（provider 不返回 logprobs 时
	// 正确做法是记未知并只依接地，不是编一个同名数）。
	Tier               string  `json:"tier"`
	ConfidenceKnown    bool    `json:"confidence_known"`    // 优先档（logprob）才有
	CalibrationProgram string  `json:"calibration_program"` // 阈值来源程序
	ThresholdVersion   string  `json:"threshold_version,omitempty"`
	Threshold          float64 `json:"threshold"` // 本次实际用的升级线（0 = 未判阈值，如强制升级）
	GapThin            bool    `json:"gap_thin"`  // 词汇鸿沟折扣是否生效（审计用）
	// 事实族（cumulus 的准确定义：事实点全盖）
	FactsK       int `json:"facts_k"`       // 拆出几条事实
	FactsCovered int `json:"facts_covered"` // 盖到几条
	FactsMissing int `json:"facts_missing"` // 没盖几条
	Conflicts    int `json:"conflicts"`     // 证据冲突数（同事实不同值）
}

// 信号档位取值。logprob 档需要 provider 返回 logprobs（本项目当前
// provider 不返回 → 恒为 retrieval；真接上时改这一个常量并重校准）。
const (
	TierLogprob   = "logprob"
	TierGrounding = "grounding"
	TierRetrieval = "retrieval"
)

// gatherSignals 从 context 采集路由信号。
func gatherSignals(c *context.Context, ws []EvidenceWindow) RouteSignals {
	sig := RouteSignals{Windows: len(ws)}
	if ci, ok := context.Get(c, KeyCoverage); ok {
		sig.Coverage = ci.Value
	}
	if len(ws) >= 2 && ws[0].Score > 0 {
		sig.Margin = (ws[0].Score - ws[1].Score) / ws[0].Score
		if sig.Margin < 0 {
			sig.Margin = 0
		}
		if sig.Margin > 1 {
			sig.Margin = 1
		}
	}
	if tel, ok := context.Get(c, KeyDeep); ok && tel.SampledDocs > 0 {
		sig.DeadRate = float64(tel.DeadEnds) / float64(tel.SampledDocs)
	}
	// 词汇鸿沟折扣：内容词几乎全党外时覆盖度再高也是假象——垃圾二元组
	// 总能凑出"有据可查"（真跑教训：问"养狗叫得太吵"，得太/谁管两个
	// 垃圾二元组匹配到垃圾文档，路由判 fast，词汇桥永远不出场）。稀薄
	// → 置信度减半，逼升级走桥。
	if rep, ok := context.Get(c, KeyFactReport); ok {
		sig.FactsK = rep.K
		sig.FactsCovered = rep.K - len(rep.Missing)
		sig.FactsMissing = len(rep.Missing)
	}
	if cs, ok := context.Get(c, KeyConflicts); ok {
		sig.Conflicts = len(cs)
	}
	// 词汇鸿沟折扣：内容词几乎全党外时覆盖度再高也是假象——垃圾二元组
	// 总能凑出"有据可查"（真跑教训：问"养狗叫得太吵"，得太/谁管两个
	// 垃圾二元组匹配到垃圾文档，路由判 fast，词汇桥永远不出场）。稀薄
	// → 置信度减半，逼升级走桥。**注意：事实族必须先填再走这个早退
	// （曾经的 bug：早退在事实填充之前，稀薄查询的信号族全是 0）**
	if an, ok := context.Get(c, KeyAnalysis); ok && an.Thin(2, 2.0) {
		sig.Confidence = RetrievalConfidence(sig) * 0.5
		sig.GapThin = true
		return sig
	}
	sig.Confidence = RetrievalConfidence(sig)
	return sig
}

// RetrievalConfidence 是**兜底档**的置信代理（原 DraftConfidence）。
//
// 改名是有意的：v0.2 §2.1 的优先档是"草稿置信度 = 首 token 对数概率"，
// 本项目 provider 不返回 logprobs（llm 包没有 logprobs 字段），所以真正
// 生效的是检索侧合成值。名字必须说出它的来历，否则响应里的 confidence
// 会被读成"模型自己的把握"——那是口径漂移，不是命名品味。
//
// 权重公开可审：覆盖度 0.5（证据到没到位的直接度量）、区分度 0.3
// （top 与次席分不开 = 没把握）、死路率 0.2（翻过多少空文档，取负）。
// 权重不是魔数，是默认值——改它要走评测对照，不靠感觉。
func RetrievalConfidence(sig RouteSignals) float64 {
	conf := 0.5*sig.Coverage + 0.3*sig.Margin + 0.2*(1-sig.DeadRate)
	if conf < 0 {
		return 0
	}
	if conf > 1 {
		return 1
	}
	return conf
}

// DraftConfidence 是 RetrievalConfidence 的旧名（保留一个版本防外部
// 引用断裂）；新代码一律用 RetrievalConfidence——名字要说出档位。
func DraftConfidence(sig RouteSignals) float64 { return RetrievalConfidence(sig) }
