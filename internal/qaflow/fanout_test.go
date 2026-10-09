package qaflow

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/retrieval"
)

// 破局件的回归：多事实问句逐事实取证据——每条事实各自检索，曾经被
// 挤出 topK 的事实证据要进窗（真跑教训：专利期限两连问——公告/授予
// 条款置顶，"期限为二十年"整条不在窗里，模型答"未涉及具体年数"）。
func TestFanoutBringsEveryFactsEvidence(t *testing.T) {
	// 长专利法：条款各异（同形重复会把密度窗口吸走——夹具要像真法条）
	patentLaw := "中华人民共和国专利法\n"
	filler := []string{
		"第一条　　为了保护专利权人的合法权益，鼓励发明创造，推动发明创造的应用，提高创新能力，促进科学技术进步和经济社会发展，制定本法。",
		"第五条　　对违反法律、社会公德或者妨害公共利益的发明创造，不授予专利权。对违反法律、行政法规的规定获取或者利用遗传资源，并依赖该遗传资源完成的发明创造，不授予专利权。",
		"第九条　　同样的发明创造只能授予一项专利权。但是，同一申请人同日对同样的发明创造既申请实用新型专利又申请发明专利，先获得的实用新型专利权尚未终止，且申请人声明放弃该实用新型专利权的，可以授予发明专利权。",
		"第十条　　专利申请权或者专利权的转让自登记之日起生效。转让专利申请权或者专利权的，当事人应当订立书面合同，并向国务院专利行政部门登记，由国务院专利行政部门予以公告。",
		"第二十条　　任何单位或者个人将在中国完成的发明向外国申请专利的，应当事先报请国务院专利行政部门进行保密审查。保密审查的程序、期限等按照国务院的规定执行。违反本条例规定向外国申请专利的发明或者实用新型，在中国申请专利的，不授予专利权。",
		"第三十条　　申请人要求优先权的，应当在申请的时候提出书面声明，并且在三个月内提交第一次提出的专利申请文件的副本；未提出书面声明或者逾期未提交专利申请文件副本的，视为未要求优先权。",
		"第三十五条　　发明专利申请自申请日起三年内，国务院专利行政部门可以根据申请人随时提出的请求，对其申请进行实质审查；申请人无正当理由逾期不请求实质审查的，该申请即被视为撤回。国务院专利行政部门认为必要的时候，可以自行对发明专利申请进行实质审查。",
		"第三十九条　　发明专利申请经实质审查没有发现驳回理由的，由国务院专利行政部门作出授予发明专利权的决定，发给发明专利证书，同时予以登记和公告。发明专利权自公告之日起生效。",
		"第四十二条　　专利期限：发明专利权的期限为二十年，实用新型专利权的期限为十年，均自申请日起计算。",
		"第四十三条　　专利权人应当自被授予专利权的当年开始缴纳年费。专利权自公告之日起生效，公告由国务院专利行政部门作出。",
	}
	for _, line := range filler {
		patentLaw += line + "\n"
	}
	// 多文档语料：别的法律也提"专利"但不提"期限/公告"——稀有度这才有
	// 区分度（单文档语料里每个词 df=1，加权无从谈起）
	docs := []retrieval.Document{{ID: "law", Body: patentLaw}}
	for i := 0; i < 6; i++ {
		docs = append(docs, retrieval.Document{
			ID:   "other" + string(rune('a'+i)),
			Body: "其他法律规定专利代理机构与专利代理师应当依法执业，专利行政部门负责全国专利管理工作。",
		})
	}
	idx := retrieval.Build(docs)
	fx := facts.Decompose("专利期限是多少年？专利权何时公告生效？")
	if len(fx) < 2 {
		t.Fatalf("两问必须拆两条事实，got %d", len(fx))
	}
	c := context.New("default")
	_ = context.Set(c, KeyFacts, fx)
	hits := retrievePerFact(c, idx, fx, 4, 240)
	if len(hits) == 0 {
		t.Fatal("fan-out 必须有窗")
	}
	// 逐事实判覆盖：合并窗后每条事实都要有自己的支撑
	views := make([]facts.Window, 0, len(hits))
	for _, h := range hits {
		views = append(views, facts.Window{SourceID: h.DocID, Span: h.SpanCoord, Text: h.SpanText, Score: h.Score})
	}
	rep := facts.Evaluate(fx, views)
	if !rep.Complete {
		t.Fatalf("fan-out 后两条事实都该有支撑，missing=%v", rep.Missing)
	}
}

// K=1 不进 fan-out（整句即事实，旧路径逐字节不变——136 问里 124 问
// K=1，常态不能为新件付出行为变化）

func TestFanoutSkipsSingleFact(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{{ID: "law", Body: "甲法\n第一条 内容。"}})
	fx := facts.Decompose("发明创造定义")
	if len(fx) != 1 {
		t.Fatalf("无并列结构必须 K=1，got %d", len(fx))
	}
	c := context.New("default")
	_ = context.Set(c, KeyFacts, fx)
	if hits := retrievePerFact(c, idx, fx, 4, 240); hits != nil {
		t.Fatalf("K=1 不许走 fan-out，got %d hits", len(hits))
	}
}

// 长文档里**单事实**问句也要按锚切窗：答案埋在 74%–98% 处，而默认窗口切在
// 48%–57% 处（真跑：长文档探针 window hit 8/10）。加宽能救但等于整篇搬运。
func TestSingleFactLongDocAnchorsWindow(t *testing.T) {
	body := "第一条 前言与适用范围。" + strings.Repeat(" filler 内容凑长度。", 120) +
		"第九十九条 专利权的期限为二十年，自申请日起计算。"
	idx := retrieval.Build([]retrieval.Document{{ID: "long", Body: body}})
	c := context.New("longdoc")
	_ = context.Set(c, KeyRewrite, Rewrite{Original: "专利权保护期限是多久"})
	_ = context.Set(c, KeyFacts, []facts.Fact{{ID: "f1", Query: "专利权 期限 二十年"}})

	hits := retrievePerFact(c, idx, []facts.Fact{{ID: "f1", Query: "专利权 期限 二十年"}}, 3, 200)
	joined := ""
	for _, h := range hits {
		joined += h.SpanText
	}
	if !strings.Contains(joined, "二十年") {
		t.Fatalf("锚定切窗应把答案句收进窗口，got %+v", hits)
	}
}

// 短文档**行为不变**：AvgLen 不够长时不做锚定切窗（零多余检索）。
func TestShortDocSkipsAnchoring(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "s1", Body: "专利权的期限为二十年，自申请日起计算。"},
		{ID: "s2", Body: "侵权赔偿按权利人损失确定。"},
	})
	if longDocAnchorable(idx, "f1") {
		t.Fatal("短文档不该触发锚定切窗（否则是无谓的多一次检索）")
	}
	// 长文档才触发
	idxLong := retrieval.Build([]retrieval.Document{{ID: "l1", Body: strings.Repeat("内容。", 500)}})
	if !longDocAnchorable(idxLong, "f1") {
		t.Fatal("长文档应触发锚定切窗")
	}
}
