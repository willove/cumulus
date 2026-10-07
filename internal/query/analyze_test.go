package query

import "testing"

type fakeCorpus struct {
	df map[string]int
}

func (f fakeCorpus) HasTerm(t string) bool { _, ok := f.df[t]; return ok }
func (f fakeCorpus) DFOf(t string) int     { return f.df[t] }

// 闯红灯如何处罚：闯/红灯 库外（鸿沟信号）；驾驶/信号 稀有（着重）；
// 处罚 满库（降权）。这正是用户点名的那组取舍。
func TestAnalyzeWeightsByCorpusIDF(t *testing.T) {
	c := fakeCorpus{df: map[string]int{
		"处罚": 900, // 库 1000 篇里的 900 篇 → 泛词，降权
		"驾驶": 3,   // 稀有 → 着重
		"信号": 40,  // 常见 → 平权
	}}
	a := Analyze("闯红灯如何处罚驾驶信号灯", c, 1000)
	if a.Intent != "search" {
		t.Fatalf("疑问句应为 search，got %s", a.Intent)
	}
	if w := a.Primary["驾驶"]; w != 2.0 {
		t.Fatalf("稀有词应着重(2.0)，got %v", w)
	}
	if w := a.Primary["信号"]; w != 1.0 {
		t.Fatalf("常见词应平权(1.0)，got %v", w)
	}
	if _, ok := a.Primary["处罚"]; ok {
		t.Fatal("泛词不该进主关键词级（降权进兜底级）")
	}
	found := false
	for _, f := range a.Fallback {
		if f == "处罚" {
			found = true
		}
	}
	if !found {
		t.Fatal("泛词应进兜底级")
	}
	hasOOV := false
	for _, o := range a.OOV {
		if o == "闯红" || o == "红灯" {
			hasOOV = true
		}
	}
	if !hasOOV {
		t.Fatalf("闯/红灯 应在语料外列表（鸿沟信号），got %v", a.OOV)
	}
}

// 主级稀薄 → Thin 为真（触发 LLM 扩展的判据）
func TestThinDetectsVocabularyGap(t *testing.T) {
	c := fakeCorpus{df: map[string]int{"处罚": 900, "如何": 800}}
	a := Analyze("闯红灯如何处罚", c, 1000)
	if !a.Thin(2, 2.0) {
		t.Fatalf("几乎全是库外/泛词时必须判稀薄: %+v", a)
	}
	// 夹具要含全部二元组才对（真实索引里全库有这些词）
	full := map[string]int{"驾驶": 3, "机动": 3, "信号": 40, "驶机": 3, "动车": 3, "车信": 40, "号灯": 2}
	rich := Analyze("驾驶机动车信号灯", fakeCorpus{df: full}, 1000)
	if rich.Thin(2, 2.0) {
		t.Fatalf("稀有权词充足且无仓外词时不该判稀薄: %+v", rich)
	}
}

// 陈述句 → chat 意图
func TestIntentChat(t *testing.T) {
	if got := intentOf("把这份文件归档"); got != "chat" {
		t.Fatalf("want chat, got %s", got)
	}
	if got := intentOf("红灯表示什么"); got != "search" {
		t.Fatalf("want search, got %s", got)
	}
}

// 胶水不进检索词：怎么/如何 这类疑问词在库内稀有也不许着重
func TestStopwordsNeverWeighted(t *testing.T) {
	c := fakeCorpus{df: map[string]int{"怎么": 2, "红灯": 5, "驾驶": 3}}
	a := Analyze("闯红灯怎么处罚", c, 1000)
	if _, ok := a.Primary["怎么"]; ok {
		t.Fatal("疑问词是胶水，不该进主关键词级（哪怕它在库里稀有）")
	}
	if _, ok := a.Primary["红灯"]; !ok {
		t.Fatal("内容词仍应在主级")
	}
}

// 语料外占比是鸿沟判据：内容词几乎全党外 → Thin，哪怕总分不低
func TestThinByOOVShare(t *testing.T) {
	c := fakeCorpus{df: map[string]int{"饲养": 3}}
	a := Analyze("养狗叫得太吵谁管", c, 1000)
	if a.OOVShare() < 0.6 {
		t.Fatalf("内容词大多在库外，OOVShare 应高，got %.2f", a.OOVShare())
	}
	if !a.Thin(2, 2.0) {
		t.Fatal("高语料外占比必须判稀薄（桥该出场）")
	}
}

// 胶水二元组降权：多少年切出的少年/限多不许着重（真跑教训：专利期限
// 多少年，返回少年相关法——垃圾二元组在库里稀有）
func TestGlueBigramsDemoted(t *testing.T) {
	c := fakeCorpus{df: map[string]int{"专利": 8, "利期": 3, "少年": 2, "限多": 1, "期限": 50}}
	a := Analyze("专利期限多少年", c, 1000)
	for _, junk := range []string{"少年", "限多"} {
		if _, ok := a.Primary[junk]; ok {
			t.Fatalf("%s 沾胶水字符，不该进主级", junk)
		}
	}
	if _, ok := a.Primary["专利"]; !ok {
		t.Fatal("专利是真内容词，该在主级")
	}
	if _, ok := a.Primary["利期"]; !ok {
		t.Fatal("利期是真内容词，该在主级")
	}
}

// 一个语料外内容词就该出桥（闯红的教训：占比 20% 过不了 60% 线，但
// 实体没被语料命名就是鸿沟本身）
func TestThinOnAnyOOV(t *testing.T) {
	// 夹具：除闯红外全在库（模拟真实索引——只有实体词没被命名）
	c := fakeCorpus{df: map[string]int{"红灯": 5, "处罚": 900, "灯怎": 40, "么处": 60, "怎么": 800}}
	a := Analyze("闯红灯怎么处罚", c, 1000)
	if a.OOVShare() >= 0.6 {
		t.Fatalf("夹具应只有一个语料外词，share=%.2f", a.OOVShare())
	}
	if !a.Thin(2, 2.0) {
		t.Fatal("有语料外实体词（闯红）就必须判稀薄")
	}
	// 对照：全库内词不该判稀薄（夹具含全部二元组）
	rich := Analyze("红灯表示什么", fakeCorpus{df: map[string]int{"红灯": 5, "表示": 60, "灯表": 30, "示什": 30, "什么": 900}}, 1000)
	if rich.Thin(2, 2.0) {
		t.Fatalf("全库内词不该判稀薄，oov=%v", rich.OOV)
	}
}
