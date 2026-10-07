// Package abstain 是零 LLM 的检索失败预测头（cumulus internal/abstain
// 的移植，形状来自 ir-rag 3.1 / RCS 的想法）。
//
// 借的是**形状**，不是别人的权重：原始论文无代码、单一作者，其权重
// 不搬。这里的 Default() 是一组保守的启发式权重（cumulus 同款取值），
// 真要拟合只能在操作员自己的样本上离线做，且**评测集永远不作训练
// 数据**（cumulus 的红线：不为刷评测题拟合阈值）。
//
// 它解决两个动作：
//   - refuse：检索期特征已经确定这题答不了（早弃权）——DEEP 的扩征在
//     这类题上是纯燃烧（cumulus 实测 ≈96s/≈19k tokens 换一个必然的拒答）；
//   - deep：看着像失败，升级再试（哪怕只有一个低分样本也可能被救回，
//     不能一刀切）。
package abstain

import "math"

// Features 全部来自一次检索尝试之后，无额外 LLM 调用。
type Features struct {
	QueryLen     int     // 查询字符数
	Candidates   int     // 进过评分管的候选数
	Kept         int     // 过线留下的窗口数
	TopScore     float64 // 最高窗分（各自量纲由调用方归一）
	MissingFacts int     // 未盖事实数（0 = 全覆盖）
	Confidence   float64 // 答案置信度 [0,1]
	Skipped      bool    // 合成没产出可用东西
	Refused      bool    // 合成因证据不足拒答
}

// Head 是线性逻辑斯蒂失败预测器。
type Head struct {
	// Weights 与 featureVec 同序（偏置另算）。
	Weights     []float64
	Bias        float64
	DeepAbove   float64 // p_fail 达线 → 走深路
	RefuseAbove float64 // p_fail 达线 → 直接拒（Refuse 优先于 Deep）
	EarlyAbove  float64 // 早弃权线：无样本且被跳过时 DEEP 前就拒
}

// Default 保守启发式（cumulus 同款取值）：特征顺序
// queryLenN, candidates, kept, topScore, missing, conf, skipped, refused。
func Default() *Head {
	return &Head{
		Weights: []float64{
			-0.15, // 查询越长越不像"空手检索失败"
			-0.05, // 候选越多越不像准入饥荒
			-0.40, // 留下的窗口是最强的成功信号
			-0.25, // 最高分
			0.55,  // 未盖事实
			-1.20, // 置信度
			1.80,  // 合成跳过
			1.70,  // 合成拒答——显式的证据不足信号
		},
		Bias:        -0.20,
		DeepAbove:   0.35,
		RefuseAbove: 0.80,
		EarlyAbove:  0.80,
	}
}

func featureVec(f Features) []float64 {
	ql := float64(f.QueryLen)
	if ql > 64 {
		ql = 64
	}
	ql /= 64
	cand := f.Candidates
	if cand > 64 {
		cand = 64
	}
	kept := f.Kept
	if kept > 32 {
		kept = 32
	}
	top := f.TopScore
	if top > 10 {
		top = 10
	}
	miss := f.MissingFacts
	if miss > 8 {
		miss = 8
	}
	conf := f.Confidence
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	return []float64{
		ql,
		float64(cand) / 8,
		float64(kept) / 4,
		top / 10,
		float64(miss) / 2,
		conf,
		boolF(f.Skipped),
		boolF(f.Refused),
	}
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// PFail 预测失败概率（logistic）。
func (h *Head) PFail(f Features) float64 {
	v := featureVec(f)
	z := h.Bias
	for i, w := range h.Weights {
		if i < len(v) {
			z += w * v[i]
		}
	}
	return 1 / (1 + math.Exp(-z))
}

// Decide 给出动作：""（无意见）/ "deep"（升级再试）/ "refuse"（早弃权）。
// EarlyAbove 只对"完全没有可用证据"（无样本且被跳过）生效：DEEP 的补
// 采样/扩征确实救回过一些题，不能一刀切——哪怕一个低分样本也升级。
func (h *Head) Decide(f Features) (p float64, act string) {
	p = h.PFail(f)
	switch {
	case p >= h.RefuseAbove:
		return p, "refuse"
	case p >= h.DeepAbove:
		return p, "deep"
	}
	if f.Kept == 0 && f.Skipped && p >= h.EarlyAbove {
		return p, "refuse"
	}
	return p, ""
}

// Verdict 是头的裁决（落 context，响应露出：取舍可查）。
type Verdict struct {
	PFail  float64 `json:"p_fail"`
	Action string  `json:"action"` // "" / "deep" / "refuse"
	Reason string  `json:"reason,omitempty"`
}
