package qaflow

import (
	"fmt"
	"os"

	gocontext "context"

	"github.com/willove/cumulus/internal/query"

	"github.com/willove/cumulus/internal/context"
)

// KeyEscalation 是升级执行的账：这一题有没有真去升级、升级后路由变成
// 了什么。升级是死标签还是有执行处，这个 key 回答得了。
var KeyEscalation = context.NewKey[EscalationRecord]("route.escalation")

// EscalationRecord 记录一次升级执行。
type EscalationRecord struct {
	Triggered bool   `json:"triggered"` // 路由是否判了 escalate
	Executed  bool   `json:"executed"`  // 是否真跑了贵路（有执行处且判了 escalate）
	Before    string `json:"before"`    // 升级前动作
	After     string `json:"after"`     // 升级后重判的动作（未执行则同 Before）
	Windows   int    `json:"windows"`   // 升级后的窗口数
	Reason    string `json:"reason,omitempty"`
	// Bridge 是词汇桥的结局（可观测）：used / rejected-score / rejected-empty /
	// weighted-error / no-bridge。**桥失手不许无声无息**——它是质量问题
	// （召回被带歪），会悄悄拉低整轮读数。
	Bridge string `json:"bridge,omitempty"`
}

// EscalateStage 是升级的执行处（BioHarness 级联：便宜快路 → 充足性
// 判定 → 贵路升级 → 再判定）。只有路由判了 escalate 才跑贵路；没判
// 就空过（留痕“未触发”）。升级后再判一次路由：
//   - 够了 → fast，带着新窗口进合成；
//   - 还不够 → refuse（"escalation exhausted"）——升级是补救机会，
//     不是绕过充足性硬答的旁门。
//
// 贵路是注入的取数函数（BM25DeepEvidence 等）——升级路径和首程同样是
// 注册件，换底物不动流程。
type EscalateStage struct {
	Retrieve func(*context.Context, Rewrite) ([]EvidenceWindow, error) // 贵路：nil = 无执行处
	// 词汇鸿沟桥（cumulus 的 keywords_multilevel）：升级且关键词级稀薄
	// 时，先把口语问句翻成语料语言的检索词，再重取。缺一个字段就只走
	// 普通贵路——桥是可选件，不是必经路。
	Expand   query.Expander
	Analyze  func(q string) query.Analysis
	Weighted func(weights map[string]float64) ([]EvidenceWindow, error)
}

func (EscalateStage) Name() string { return "escalate" }

// Available 报告这一层**有没有执行处**（贵路或桥）。路由在"零窗口要不要先升级"
// 上要复核它：配置声明有、但实际没装 = 空转，那还是直接拒答。
func (s EscalateStage) Available() bool {
	return s.Retrieve != nil || (s.Expand != nil && s.Weighted != nil)
}
func (EscalateStage) Reads() []string {
	return []string{KeyRoute.String(), KeyRewrite.String()}
}
func (EscalateStage) Writes() []string {
	return []string{KeyWindows.String(), KeyRoute.String(), KeyEscalation.String(), KeyDeep.String(), KeyRerank.String(), KeyCoverage.String()}
}

func (s EscalateStage) Run(c *context.Context) error {
	route, _ := context.Get(c, KeyRoute)
	rec := EscalationRecord{Triggered: route.Action == "escalate", Before: route.Action, After: route.Action}
	if !rec.Triggered {
		rec.Reason = "route did not escalate"
		return context.Set(c, KeyEscalation, rec)
	}
	if s.Retrieve == nil {
		rec.Reason = "no escalation backend wired; escalate stays terminal"
		return context.Set(c, KeyEscalation, rec)
	}
	rw, ok := context.Get(c, KeyRewrite)
	if !ok {
		return fmt.Errorf("escalate: rewrite missing")
	}
	// 先把初始记录写进 context：桥的结局要记在**同一条**记录上，而 escalateRetrieve
	// 是在它之前跑的（真跑踩过：结局恒空——noteBridge 当时无处可写）。
	if err := context.Set(c, KeyEscalation, rec); err != nil {
		return err
	}
	ws, err := s.escalateRetrieve(c, rw)
	if err != nil {
		return fmt.Errorf("escalate: retrieve: %w", err)
	}
	// **合并**而不是替换：首程窗（再问时已加宽）与贵路窗合起来才够判。
	// 替换是自相矛盾的——首程刚逐事实取回的证据，一替换就把事实的支撑
	// 窗丢了（真跑教训：再问后 f1 的覆盖反而从"盖"变"缺"，模型答"该事
	// 实无支撑证据"）。合并后按 (源,span) 去重、按分降序、封顶 12——
	// 再多对合成面就是噪声（提示词预算）。
	prev, _ := context.Get(c, KeyWindows)
	merged := mergeWindows(prev, ws)
	if err := context.Set(c, KeyWindows, merged); err != nil {
		return err
	}
	// 取回 escalateRetrieve 期间 noteBridge 记的结局：局部的 rec 是**调用前的
	// 快照**，直接收尾会把桥的记录覆盖掉（真跑踩过：护栏行为对、记录却空）。
	if latest, ok := context.Get(c, KeyEscalation); ok && latest.Bridge != "" {
		rec.Bridge = latest.Bridge
	}
	rec.Executed = true
	rec.Windows = len(ws)
	// 重判：升级后的窗口重新算信号（升级前后的路由判据同一把尺）
	next := routeWith(c, ws)
	rec.After = next.Action
	if err := context.Set(c, KeyRoute, next); err != nil {
		return err
	}
	return context.Set(c, KeyEscalation, rec)
}

func (EscalateStage) Verify(c *context.Context) error {
	// 升级后仍然拒答的话，答案不许出现（Verify 在合成 stage 兜同一道）
	return nil
}

// routeWith 用当前 context 里的信号对给定窗口重算路由（升级后重判用）。
//
// 升级后判据和首程不同，这是有意的：**升级的钱已经花了**。首程 escalate
// 的门槛可以低（不确定就多看一眼），升级后还以同一个低门槛拒答，等于把
// 刚花的钱扔掉（真跑实测：65 题升级后仍以覆盖不足拒答，但金标就在窗内
// ——79.3% 的证据命中被 21.7% 的拒答抵消）。所以升级后只有“贵路什么都没
// 取到”才拒答；取到了就作答，但把残余低置信度写进 reason（诚实标注：
// 这题是带疑作答，不是干净命中）。
func routeWith(c *context.Context, ws []EvidenceWindow) RouteDecision {
	sig := gatherSignals(c, ws)
	// 档位与校准来源是**装配的事实**，不是本轮窗口的函数：重判换的是窗口
	// 与结论，不换档位、程序和阈值版本。丢了它们，升级过的题在响应与
	// 提交视图里就变成"来历不明"——审计上等于这次判定没有校准出处
	// （真跑抓到过：serve 里凡是走过升级的题，committed.calibration 全空）。
	if prev, ok := context.Get(c, KeyRoute); ok {
		sig.Tier = prev.Signals.Tier
		sig.ConfidenceKnown = prev.Signals.ConfidenceKnown
		sig.CalibrationProgram = prev.Signals.CalibrationProgram
		sig.ThresholdVersion = prev.Signals.ThresholdVersion
		sig.Threshold = prev.Signals.Threshold
	}
	d := RouteDecision{Grounded: len(ws) > 0, Signals: sig}
	switch {
	case len(ws) == 0:
		d.Action = "refuse"
		d.Reason = "no evidence window after escalation; refuse by grammar"
	case sig.Confidence < 0.5:
		d.Action = "fast"
		d.Reason = fmt.Sprintf("answered after escalation with residual low confidence %.3f (coverage=%.3f margin=%.3f) — best-effort, not a clean hit", sig.Confidence, sig.Coverage, sig.Margin)
	default:
		d.Action = "fast"
		d.Reason = fmt.Sprintf("resolved by escalation: confidence %.3f (coverage=%.3f margin=%.3f)", sig.Confidence, sig.Coverage, sig.Margin)
	}
	return d
}

// escalateRetrieve 升级时的取数：关键词级稀薄（词汇鸿沟）且有桥时，先
// 扩展再按权取；否则走普通贵路。扩展失败不阻塞——按原查询走贵路，
// 原因由调用方日志承担，不许把流程搞死。
func (s EscalateStage) escalateRetrieve(c *context.Context, rw Rewrite) ([]EvidenceWindow, error) {
	a, hasAnalysis := context.Get(c, KeyAnalysis)
	thin := !hasAnalysis || a.Thin(2, 2.0)
	// 桥的触发判定必须**看得见**：三臂对照全读"未走桥"时，靠猜会连错四次
	// （语料/开关/装配/路由各猜一遍），真正的答案是"thin 没成立"。
	if os.Getenv("CUMULUS_BRIDGE_DEBUG") == "1" {
		fmt.Printf("bridge-debug: analysis=%v primary=%d oov=%d thin=%v expand=%v weighted=%v\n",
			hasAnalysis, len(a.Primary), len(a.OOV), thin, s.Expand != nil, s.Weighted != nil)
	}
	if s.Expand == nil || s.Weighted == nil || !thin {
		s.noteBridge(c, "skipped:"+bridgeSkipReason(s, thin))
		return s.Retrieve(c, rw)
	}
	// 注意下面那条"加权 0 命中 → 退回朴素贵路"的兜底：桥扩偏时加权重取可能
	// 一条都取不到，而**0 命中不等于语料里没有证据**——把它当成"不存在"就会
	// 让本来答得上来的题被拒答（真跑踩过：单文档语料 margin=0 必升级，桥一扩偏
	// 就 0 窗，于是 facts 有支撑、分数 12.8 仍然拒答）。
	expanded, err := s.Expand.Expand(gocontext.Background(), rw.Original)
	if err != nil || len(expanded) == 0 {
		return s.Retrieve(c, rw) // 桥失败：退化贵路
	}
	// 合并权重：**扩展词为主，原主级降为 0.2 的边注**。桥的语义是替换不
	// 是并列——原问词在鸿沟场景下正是失败的那批（垃圾二元组在库里稀
	// 有、idf 高，并列着会把桥带歪：真跑教训"养狗叫得太吵"扩出噪声，
	// 得太/谁管俩垃圾词权重 2.0/1.0 把窗口拉去太湖流域管理条例）。
	// 原词不清零：非纯鸿沟时它们仍可能带对信号。
	weights := map[string]float64{}
	for k, v := range a.Primary {
		weights[k] = v * 0.2
	}
	for _, k := range expanded {
		// 扩展词拆成索引口径（二元组）才进得了倒排
		for _, bg := range query.SplitTerms(k) {
			// 扩展词覆盖同形原词（桥比原问词懂行——重叠时听桥的）
			weights[bg] = 2.0
		}
	}
	ws, werr := s.Weighted(weights)
	if werr != nil {
		s.noteBridge(c, "weighted-error")
		return s.Retrieve(c, rw) // 加权路出错：退化朴素贵路（不是"没证据"）
	}
	// **桥必须证明自己有用**（本轮加的护栏）：拿朴素路做对照，分明显更低就弃用桥。
	//
	// 为什么必须对照：桥扩偏时**不会返回 0 命中**——它会返回"很自信的噪声窗口"
	//（上一轮的真跑教训："养狗叫得太吵"扩出噪声，窗口被拉去太湖流域管理条例）。
	// 0 命中兜底抓不住这种情况，只有与原问的直接对照抓得住。
	//
	// BM25 一次检索是微秒级，多跑一次朴素路几乎不要钱；比"桥把检索带歪"的代价
	// （整轮答错方向）便宜几个数量级。
	plain, perr := s.Retrieve(c, rw)
	if perr == nil && bridgeGuardEnabled() && bridgeWorse(ws, plain, BridgeScoreRatio) {
		s.noteBridge(c, "rejected-score")
		return plain, nil
	}
	if len(ws) == 0 {
		// **0 命中不等于不存在**。桥可能把问句扩到与语料毫无交集的方向
		// （真跑教训："养狗叫得太吵"扩出噪声；这里的单文档问句同样被扩偏），
		// 此时给朴素路一次机会：桥是**增强**，它失手不该等于系统失忆。
		s.noteBridge(c, "rejected-empty")
		return plain, nil
	}
	s.noteBridge(c, "used")
	return ws, nil
}

// BridgeScoreRatio 是**弃用门槛**：加权路的首窗分低于朴素路首窗分的这个比例，
// 就认为桥把检索带歪了（0 = 桥必须更优才留）。
//
// 0.75 而不是 1.0：桥的召回面更大，同一批文档上的最高分**理应**不低于朴素路；
// 留 25% 容忍度是为了不因"桥召回更好但最高分略低"这种正常情况把桥毙掉。
// 语料/桥表现变化时这个数要按数据调（它是一个假设，不是真理）。
const BridgeScoreRatio = 0.75

// bridgeWorse 判断加权路是否明显不如朴素路（只看**首窗分**，不比较窗数：
// 桥的价值是召回更宽，用窗数判会把"召回更好"误判成"更差"）。
func bridgeWorse(weighted, plain []EvidenceWindow, ratio float64) bool {
	if len(weighted) == 0 || len(plain) == 0 {
		return false // 交给 0 命中兜底
	}
	return weighted[0].Score < plain[0].Score*ratio
}

// noteBridge 把桥的结局记进升级记录（可观测：桥失手不许无声无息）。
//
// 为什么记在这里而不是日志：桥失手是**质量问题**（召回被带歪），不是运行事故；
// 日志会被刷掉，而 EscalationRecord 会进每题结果、能进对比与回归读数。
func (s EscalateStage) noteBridge(c *context.Context, outcome string) {
	rec, ok := context.Get(c, KeyEscalation)
	if !ok {
		return
	}
	if rec.Bridge == "" {
		rec.Bridge = outcome
	} else {
		rec.Bridge += "+" + outcome
	}
	_ = context.Set(c, KeyEscalation, rec)
}

// mergeWindows 合并两个窗集：按 (SourceID,Span) 去重，按分降序，封顶
// 12（合成提示词的预算上限）。
func mergeWindows(a, b []EvidenceWindow) []EvidenceWindow {
	seen := map[string]bool{}
	out := make([]EvidenceWindow, 0, len(a)+len(b))
	for _, ws := range [][]EvidenceWindow{a, b} {
		for _, w := range ws {
			key := w.SourceID + "#" + w.Span
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, w)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Score > out[j-1].Score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// bridgeGuardEnabled 读 CUMULUS_BRIDGE_GUARD（默认开；=0 关护栏，只用于消融实验）。
//
// 为什么护栏本身也要能关：它是**假设**（桥的首窗分不该明显低于朴素路）。要验证这个
// 假设，就必须能在同一份数据上关掉它跑一遍——不能验证的护栏不如没有。
func bridgeGuardEnabled() bool { return os.Getenv("CUMULUS_BRIDGE_GUARD") != "0" }

// bridgeSkipReason 说清桥为什么没上场（可观测：桥"没走"要有原因，不能只说没走）。
func bridgeSkipReason(s EscalateStage, thin bool) string {
	switch {
	case s.Expand == nil:
		return "no-expander"
	case s.Weighted == nil:
		return "no-weighted"
	case !thin:
		return "not-thin"
	default:
		return "none"
	}
}
