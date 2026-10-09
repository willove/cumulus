package retrieval

// tier_measure_test.go —— 分层的**量化验证**（离线、确定性、不依赖 LLM）。
//
// 为什么要离线：分层唯一的真实代价是"冷路径要多读几次盘"，测它不需要模型——
// 模型只会把噪声加进延迟读数。召回对比同理：同一套 BM25，唯一变量是分层。
//
// 跑：CUMULUS_MEASURE=1 go test ./internal/retrieval/ -run TestTierMeasure -v

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// synthCorpus 造 n 篇**词形有变化**的语料（第 i 篇取若干条法条段 + 自己的编号），
// 倒排规模接近真实文档，而不是复制同一段（复制品词形重复，会低估倒排与指纹）。
func synthCorpus(n int) []Document {
	arts := []string{
		"专利权期限为二十年，自申请日起计算。",
		"未在规定期限内缴纳年费的，应当补缴并加收滞纳金。",
		"犬只外出必须由成年人牵领，并佩戴犬牌。",
		"居民饲养犬只外出应当由成年人牵领并佩戴犬牌。",
		"生活垃圾应当分类投放，定时定点为每日七时至九时。",
		"用人单位应当与劳动者签订书面劳动合同。",
		"劳动者连续工作满十年的应当订立无固定期限合同。",
		"著作权人应当按照约定支付报酬。",
		"为学校课堂教学可以少量复制已经发表的作品。",
		"软件著作权登记应当提交源代码鉴别材料。",
		"专利权人享有实施、许可他人实施以及转让专利的权利。",
		"专利实施许可合同应当采用书面形式。",
	}
	docs := make([]Document, 0, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, "第%d章 主题%d的说明。\n", i, i%37)
		for k := 0; k < 3; k++ {
			fmt.Fprintf(&b, "%s（%d-%d）\n", arts[(i+k*5)%len(arts)], i, k)
		}
		fmt.Fprintf(&b, "本篇编号%d，其他事项按第%d条处理。", i, i%11)
		docs = append(docs, Document{ID: fmt.Sprintf("m%06d", i), Body: b.String()})
	}
	return docs
}

func pct(t *testing.T, name string, d []time.Duration) {
	if len(d) == 0 {
		return
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	p := func(q float64) time.Duration {
		i := int(q * float64(len(d)-1))
		return d[i]
	}
	t.Logf("  %-22s n=%-5d p50=%-10v p95=%-10v p99=%v",
		name, len(d), p(0.5).Round(time.Microsecond), p(0.95).Round(time.Microsecond), p(0.99).Round(time.Microsecond))
}

// TestTierMeasure 量三件事：① 冷路径慢多少 ② 召回是否与全内存一致 ③ 指纹假阳性率
func TestTierMeasure(t *testing.T) {
	if os.Getenv("CUMULUS_MEASURE") != "1" {
		t.Skip("设 CUMULUS_MEASURE=1 才跑（它要几十秒）")
	}
	loads := map[string]int{}
	loader := func(docs []Document) Loader {
		m := map[string]string{}
		for _, d := range docs {
			m[d.ID] = d.Body
		}
		return func(id string) (string, bool) {
			loads[id]++
			b, ok := m[id]
			return b, ok
		}
	}

	for _, n := range []int{500, 2000, 5000} {
		docs := synthCorpus(n)
		idx := Build(docs)
		entries := 0
		for _, ps := range idx.Postings {
			entries += len(ps)
		}
		fullEntries := entries

		const budget = 200
		tiered := BuildTiered(docs, loader(docs), TierPolicy{HotDocs: budget})

		// ① 延迟：热命中 vs 冷命中（同一批查询，分开统计）
		hotQ := make([]string, 0, 50)
		coldQ := make([]string, 0, 50)
		for i := 0; i < n && len(hotQ)+len(coldQ) < 100; i++ {
			q := fmt.Sprintf("编号%d 其他事项", i)
			if i < budget && len(hotQ) < 50 {
				hotQ = append(hotQ, q)
			} else if i >= budget && len(coldQ) < 50 {
				coldQ = append(coldQ, q)
			}
		}
		timeIt := func(ix *Index, qs []string) []time.Duration {
			for _, q := range qs { // 预热（首次调用含懒初始化）
				ix.Search(q, 9, 400)
			}
			out := make([]time.Duration, 0, len(qs)*5)
			for r := 0; r < 5; r++ {
				for _, q := range qs {
					st := time.Now()
					ix.Search(q, 9, 400)
					out = append(out, time.Since(st))
				}
			}
			return out
		}
		hotD := timeIt(tiered, hotQ)
		coldD := timeIt(tiered, coldQ)
		fullD := timeIt(idx, append(append([]string{}, hotQ...), coldQ...))

		// ② 召回：**金标文档在不在 top-9**（不是"结果集逐条相同"）
		//
		// 口径更正（真跑教训）：我第一版比"两次结果集完全一致"，而中文语料里大量
		// 文档分数**完全相同**（都含"编号/其他/事项"），于是差异全部来自平局取舍、
		// 与召回无关——那个指标测的是噪声。**该测的是：金标在不在。**
		goldInFull, goldInTiered, regressions, ties := 0, 0, 0, 0
		for _, q := range append(append([]string{}, hotQ...), coldQ...) {
			// 金标 = **问句里那个数字**对应的文档（真跑教训：我第一版用"查询下标"当
			// 编号，于是金标成了 0.004 分的噪声填充项，量出"分层丢 3 条"——其实两边的
			// top-1 都完全正确，差异只在同为 0 分的填充项里。**测量工具自己算错金标，
			// 比系统出错更难发现**，因为它给出一个像模像样的坏数字）。
			gold := goldOf(q)
			a, b := idx.Search(q, 9, 400), tiered.Search(q, 9, 400)
			inA, inB := false, false
			for _, h := range a {
				if h.DocID == gold {
					inA = true
				}
			}
			for _, h := range b {
				if h.DocID == gold {
					inB = true
				}
			}
			if inA {
				goldInFull++
			}
			if inB {
				goldInTiered++
			}
			if inA && !inB {
				regressions++
				if regressions <= 3 {
					var ranked []string
					for _, h := range a {
						ranked = append(ranked, fmt.Sprintf("%s:%.3f", h.DocID, h.Score))
					}
					var tb []string
					for _, h := range b {
						tb = append(tb, fmt.Sprintf("%s:%.3f", h.DocID, h.Score))
					}
					t.Logf("    ↳ 丢金标 %s（查询 %q）· 全内存: %v · 分层: %v", gold, q, ranked, tb)
				}
			}
			// 平局程度：全内存结果里有多少条与首条同分（分不出来 = 取舍是任意的）
			if len(a) > 1 && a[0].Score > 0 {
				same := 0
				for _, h := range a[1:] {
					if h.Score > a[0].Score-1e-9 {
						same++
					}
				}
				if same > 0 {
					ties++
				}
			}
		}

		// ③ 指纹假阳性：候选里"其实一个查询词都没有"的占比
		terms := UniqueTerms(Fields(coldQ[0]))
		cands := sketchCandidates(tiered.tier.sketches, tiered.DocLens, terms, 9)
		fp := 0
		for _, c := range cands {
			real := false
			for _, d := range docs {
				if d.ID != c.id {
					continue
				}
				set := map[string]bool{}
				for _, x := range Fields(d.Body) {
					set[x] = true
				}
				for _, tm := range terms {
					if set[tm] {
						real = true
						break
					}
				}
				break
			}
			if !real {
				fp++
			}
		}
		st := tiered.TierStats()
		t.Logf("语料 %d 篇（倒排 %d 条）热区 %d", n, fullEntries, budget)
		pct(t, "全内存 p50/p95", fullD)
		pct(t, "分层·热命中", hotD)
		pct(t, "分层·冷命中", coldD)
		t.Logf("  金标在 top9：全内存 %d/%d · 分层 %d/%d · **分层丢的 %d 条** · 有平局的查询 %d",
			goldInFull, len(hotQ)+len(coldQ), goldInTiered, len(hotQ)+len(coldQ), regressions, ties)
		t.Logf("  冷候选 %d 条，其中假阳性 %d 条（%.0f%%）· 升权 %d · 降权 %d",
			len(cands), fp, 100*float64(fp)/float64(math_max(1, len(cands))), st.Promoted, st.Demoted)
	}
}

// goldOf 从查询里取出金标文档 id（问句形如「编号200 其他事项」）。
func goldOf(q string) string {
	d := digitsOf(q)
	if d == "" {
		return ""
	}
	return fmt.Sprintf("m%06d", atoiOf(d))
}

func digitsOf(s string) string {
	out := ""
	for _, r := range s {
		if r >= '0' && r <= '9' {
			out += string(r)
		} else if out != "" {
			break
		}
	}
	return out
}

func atoiOf(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func math_max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
