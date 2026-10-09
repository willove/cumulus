package retrieval

import (
	"strings"
	"testing"
)

// 长文档里**同一篇**要取多个段位窗口：BM25 每篇只有一个分数，所以"每篇一窗"在长文里
// 必然漏——真跑：答案在 98% 处、词密度与开头段并列最高，却完全没被覆盖。
func TestWindowMultiSpansCoversDistantEvidence(t *testing.T) {
	// 文档要**够长**（≥3 个段宽），否则"每篇一窗"的退化正好是对的（见下面的短文档用例）
	body := "第一条 宣告专利权无效应当提交无效宣告请求书。" + strings.Repeat("审查员应当说明理由，据此备案。", 30) +
		"第三十条 宣告无效的专利权视为自始不存在。"
	idx := Build([]Document{{ID: "d1", Body: body}})
	terms := []string{"宣告", "无效", "专利"}
	hits := idx.WindowMultiSpans("d1", terms, 200, 2)
	if len(hits) < 2 {
		t.Fatalf("长文档应取多个段位窗口，实际 %d", len(hits))
	}
	joined := ""
	for _, h := range hits {
		joined += h.SpanText
	}
	if !strings.Contains(joined, "视为自始不存在") {
		t.Fatalf("末尾段的证据必须被覆盖（这是长文档最常见的漏法）: %+v", hits)
	}
	// 段位窗口之间**不得重叠**（否则等于把同一段重复给下游）
	for i := 1; i < len(hits); i++ {
		_, prevHi := spanBounds(hits[i-1].SpanCoord)
		curLo, _ := spanBounds(hits[i].SpanCoord)
		if curLo < prevHi {
			t.Fatalf("段位窗口重叠: %s vs %s", hits[i-1].SpanCoord, hits[i].SpanCoord)
		}
	}
}

// 短文档**退化成原来那一个窗口**（不制造多余窗口）。
func TestWindowMultiSpansDegradesOnShortDocs(t *testing.T) {
	idx := Build([]Document{{ID: "s", Body: "宣告专利权无效的，视为自始不存在。"}})
	hits := idx.WindowMultiSpans("s", []string{"宣告", "无效"}, 400, 3)
	if len(hits) != 1 {
		t.Fatalf("短文档应只取一个窗口（零多余开销），实际 %d: %+v", len(hits), hits)
	}
}

// 没有命中的文档不给窗口（别拿"空窗口"充数）。
func TestWindowMultiSpansNoMatchGivesNothing(t *testing.T) {
	idx := Build([]Document{{ID: "d", Body: strings.Repeat("无关内容。", 200)}})
	if hits := idx.WindowMultiSpans("d", []string{"宣告", "专利权"}, 200, 2); len(hits) != 0 {
		t.Fatalf("无命中应返回空: %+v", hits)
	}
}

func spanBounds(coord string) (int, int) {
	// "rune[12:345]" → 12, 345
	i := strings.Index(coord, "[")
	j := strings.Index(coord, "]")
	if i < 0 || j < 0 {
		return 0, 0
	}
	parts := strings.SplitN(coord[i+1:j], ":", 2)
	var lo, hi int
	for _, p := range parts {
		n := 0
		for _, r := range p {
			if r >= '0' && r <= '9' {
				n = n*10 + int(r-'0')
			}
		}
		if len(parts) == 2 && lo == 0 {
			lo = n
		} else {
			hi = n
		}
	}
	return lo, hi
}

// 段位窗口**扩展后也不得与已取窗口重叠**（真踩过：第二窗扩成 rune[68:468]，
// 与第一窗 rune[0:400] 重叠 332 字——"多段位"只是把同一段给了两遍，还白花 token）。
func TestWindowMultiSpansNoOverlapAfterExpansion(t *testing.T) {
	// 每段都命中、都差不多密 → 贪心会连续取相邻两段，扩展后必然撞车
	body := strings.Repeat("宣告无效专利权的规定。", 40)
	idx := Build([]Document{{ID: "d", Body: body}})
	hits := idx.WindowMultiSpans("d", []string{"宣告", "无效", "专利"}, 400, 2)
	for i := 1; i < len(hits); i++ {
		_, prevHi := spanBounds(hits[i-1].SpanCoord)
		curLo, curHi := spanBounds(hits[i].SpanCoord)
		if curLo < prevHi {
			t.Fatalf("扩展后仍重叠: %s vs %s", hits[i-1].SpanCoord, hits[i].SpanCoord)
		}
		if curHi <= curLo {
			t.Fatalf("窗口不应为空: %s", hits[i].SpanCoord)
		}
	}
}
