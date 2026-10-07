package evalfcore

import "fmt"

// 本文件是阈值校准的读数面（v0.2 §2.1：阈值不手调，两条来源）。
//
// 它只负责**把校准要的两个读数摆出来**，不替人做决定：
//   - 可靠性曲线：置信代理分桶后的可观测正确率（单调性成立才谈得上
//     把置信度当风险代理）；
//   - τ₀ 候选：强臂（DEEP）的经验准确率（CAUC 口径）。
//
// 正确性口径的选择有讲究：优先用判官（JudgeAcc），判官缺席时退到
// 证据命中率（EvidenceHitRate）。**判官给自己当尺要留神**——判官自己
// 也要校准（口径从"等价"改"支持"就是一次校准），拿它当唯一事实源
// 等于让被评系统自造判据（综述 §4.3.2 的 FlowGen 教训）。所以口径名
// 随读数一起返回，谁也别假装它是普适真理。

// Bucket 是一个置信分桶。Accuracy 用可观测正确性（见文件头）。
type Bucket struct {
	Lo       float64
	Hi       float64
	N        int
	Accuracy float64 // 桶内正确率
	Escalate int     // 桶内被判升级的题数（阈值该画在哪的直接读数）
	Refused  int     // 桶内拒答数（过度弃权的读数）
}

// CalibrationTable 按 Confidence 分桶（buckets<=0 用 5）。执行失败项
// （eval-error）不进校准——那不是模型行为，是运维噪声。
func CalibrationTable(s RunState, buckets int) []Bucket {
	if buckets <= 0 {
		buckets = 5
	}
	out := make([]Bucket, buckets)
	for i := range out {
		out[i].Lo = float64(i) / float64(buckets)
		out[i].Hi = float64(i+1) / float64(buckets)
	}
	for _, r := range s.Results {
		if isEvalError(r.Failure) {
			continue
		}
		idx := int(r.Confidence * float64(buckets))
		if idx >= buckets {
			idx = buckets - 1
		}
		if idx < 0 {
			idx = 0
		}
		out[idx].N++
		if r.EvidenceHit {
			out[idx].Accuracy++
		}
		if r.RouteAction == "escalate" {
			out[idx].Escalate++
		}
		if r.Refused {
			out[idx].Refused++
		}
	}
	for i := range out {
		if out[i].N > 0 {
			out[i].Accuracy /= float64(out[i].N)
		}
	}
	return out
}

func isEvalError(f string) bool { return len(f) > 9 && f[:10] == "eval-error" }

// CalibrationMonotone 检查分桶正确率单调不减（忽略空桶）。这是"置信度
// 可当风险代理"的最低要求——不单调就不许拿它画阈值。
func CalibrationMonotone(bs []Bucket) bool {
	prev := -1.0
	for _, b := range bs {
		if b.N == 0 {
			continue
		}
		if b.Accuracy < prev {
			return false
		}
		prev = b.Accuracy
	}
	return true
}

// SuggestedTau0 给出 CAUC 口径的升级线候选：强臂的经验准确率。
// 返回（值, 口径名）。判官在场优先用判官口径——判官口径更接近"答案
// 对不对"，证据命中率只是"证据到没到"。
//
// 分母只算**有效题**：执行失败（eval-error）是运维噪声，不该拉低强臂
// 的准确率读数——用它画阈值等于把网络抖动当模型能力。
func SuggestedTau0(s RunState) (float64, string) {
	var valid, hits, judgeN, judgeOK int
	for _, r := range s.Results {
		if isEvalError(r.Failure) {
			continue
		}
		valid++
		if r.EvidenceHit {
			hits++
		}
		if r.JudgeOK != nil {
			judgeN++
			if *r.JudgeOK {
				judgeOK++
			}
		}
	}
	if valid == 0 {
		return 0, "no-valid-items"
	}
	if judgeN > 0 {
		return float64(judgeOK) / float64(judgeN), "judge-accuracy"
	}
	return float64(hits) / float64(valid), "evidence-hit-rate"
}

// CalibrationReport 渲染校准读数（CLI 用）。
func CalibrationReport(arm string, s RunState) string {
	bs := CalibrationTable(s, 5)
	tau, oracle := SuggestedTau0(s)
	head := fmt.Sprintf("calibration[%s] tau0=%.3f (%s) monotone=%v", arm, tau, oracle, CalibrationMonotone(bs))
	for _, b := range bs {
		if b.N == 0 {
			continue
		}
		head += fmt.Sprintf("\n  conf[%.1f,%.1f) n=%-4d acc=%.2f escalate=%d refused=%d", b.Lo, b.Hi, b.N, b.Accuracy, b.Escalate, b.Refused)
	}
	return head
}
