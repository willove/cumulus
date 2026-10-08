package qaflow

import "testing"

// 带符号支持看的是"答案有没有证据"，不是"窗口像不像问题"：
// 答案里的内容词落在窗口里越多，支持越高；胶水/停用词不进分母。
func TestAnswerSupportMeasuresAnswerEvidence(t *testing.T) {
	ws := []EvidenceWindow{
		{SourceID: "d1", Text: "连接池最大连接数默认为 100，超过需调整配置。"},
	}
	// 注意不是恰好 1.0：问句形态会带出一个跨词二元组"数是"（是 不在
	// query 的胶水字符里），它不在窗口原文中，于是 5/6=0.83。**一个胶水
	// 口径**（query.IsGlue）是刻意的：为了这个信号再养一套停用词表，
	// 就是两处口径漂移；0.83 与 1.0 在"答案词汇来自窗口"这件事上同义。
	full, n := AnswerSupport("最大连接数是 100", ws)
	if full < 0.8 || n == 0 {
		t.Fatalf("answered from the window must have high support: %v (n=%d)", full, n)
	}
	none, _ := AnswerSupport("季度收入增长三成", ws)
	if none != 0 {
		t.Fatalf("answer with no evidence must have zero support, got %v", none)
	}
	// 分母只随内容词长：疑问胶水（"多少"）不进分母；但跨词二元组"数是"
	// 会进（"是"不在 query 的胶水字符里）。这里**把事实钉死**而不是放宽
	// 断言：谁要是改了胶水词表，这条会红——改口径要看见代价，不许悄悄改。
	_, qN := AnswerSupport("连接池最大连接数是多少", ws)
	_, pN := AnswerSupport("连接池最大连接数", ws)
	if qN != pN+1 {
		t.Fatalf("only the copula bigram may remain in the denominator: %d vs %d", qN, pN)
	}
	if v, n := AnswerSupport("的呢吧", ws); v != 0 || n != 0 {
		t.Fatalf("pure glue answer has no verifiable claim: v=%v n=%d", v, n)
	}
	if v, n := AnswerSupport("最大连接数", nil); v != 0 || n != 0 {
		t.Fatalf("no windows means no support: v=%v n=%d", v, n)
	}
}
