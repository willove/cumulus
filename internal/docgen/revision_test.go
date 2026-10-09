package docgen

import (
	"fmt"
	"strings"
	"testing"
)

func docOf(claims ...string) *Document {
	d := &Document{Version: 1}
	for _, c := range claims {
		d.Sections = append(d.Sections, Section{Heading: "H",
			Claims: []Claim{{Text: c, SourceID: "d1"}}})
	}
	return d
}

// 同主题 → 同一篇文档（**覆盖**而不是新增）。这是"不堆重复"的结构保证。
func TestDocIDIsStablePerTopic(t *testing.T) {
	a := DocID("专利年费怎么交")
	b := DocID("  专利年费怎么交  ") // 空白差异
	if a != b {
		t.Fatalf("空白差异必须归一到同一个 id: %s vs %s", a, b)
	}
	// 全角/半角标点等价（**查表**口径，不是算术——见 NormalizeTopic 注释）
	if DocID("专利年费怎么交？") != DocID("专利年费怎么交?") {
		t.Fatal("全角与半角标点必须等价（否则同一件事的两种标点生成两篇文档）")
	}
	// 内容不同的主题必须是不同 id（归一化不等于抹平一切）
	if DocID("专利年费怎么交") == DocID("专利年费逾期怎么办") {
		t.Fatal("不同主题必须是不同 id")
	}
	if !IsGenID(a) {
		t.Fatalf("生成文档 id 必须带 gen- 前缀（检索期靠它降权）: %s", a)
	}
}

func TestReviseReportsAdditionsAndRemovals(t *testing.T) {
	old := docOf("年费三百八十元", "逾期加滞纳金")
	new := docOf("年费三百八十元", "逾期加滞纳金", "可补缴六个月")

	rev := Revise(old, new, []string{"d1"}, []string{"d1"})
	if rev.Version != 2 {
		t.Fatalf("version must bump: %+v", rev)
	}
	if len(rev.Added) != 1 || rev.Added[0] != "可补缴六个月" {
		t.Fatalf("added wrong: %+v", rev.Added)
	}
	if len(rev.Removed) != 0 {
		t.Fatalf("nothing removed: %+v", rev.Removed)
	}
	if rev.Note() != "新增 1 条" {
		t.Fatalf("note wrong: %q", rev.Note())
	}
	if rev.SourcesChanged {
		t.Fatal("sources unchanged → SourcesChanged false")
	}

	// 反向：新版少了内容 → 必须显式说"移除"（读者要警觉）
	back := Revise(new, old, []string{"d1"}, []string{"d1"})
	if len(back.Removed) != 1 || back.Removed[0] != "可补缴六个月" {
		t.Fatalf("removal must be reported: %+v", back)
	}
	if got := back.Note(); got == "" || got == "内容与上一版一致" {
		t.Fatalf("信息损失不许说成'一致': %q", got)
	}
}

// 首次生成是 v1，且**不写版本标记**（没有"上一版"可言，写了就是噪声）。
func TestFirstVersionHasNoStamp(t *testing.T) {
	rev := Revise(nil, docOf("x"), nil, []string{"d1"})
	if rev.Version != 1 {
		t.Fatalf("first version must be 1: %+v", rev)
	}
	if Stamp("body", rev) != "body" {
		t.Fatal("v1 must not be stamped")
	}
	v2 := Revise(docOf("x"), docOf("x", "y"), nil, nil)
	stamped := Stamp("body", v2)
	// v2 的标记带版本与增量说明（`<!-- cumulus:generated v2 · 新增 1 条 · 日期 -->`），
	// 所以不能拿 v1 的 marker 字面比较——查"带了版本号"这个事实。
	if stamped == "body" || !strings.Contains(stamped, "cumulus:generated v2") {
		t.Fatalf("v2 must carry the marker with version/note: %q", stamped)
	}
}

// 内容完全一致的再生成：说"一致"，**不要假装有更新**。
func TestRegenerateWithNoChangeSaysSo(t *testing.T) {
	d := docOf("a", "b")
	rev := Revise(d, docOf("a", "b"), nil, nil)
	if rev.Version != 2 {
		t.Fatalf("版本仍然要递增（读者要知道它被重新生成过）: %+v", rev)
	}
	if rev.Note() != "内容与上一版一致（本次只是重新生成）" {
		t.Fatalf("note must be honest: %q", rev.Note())
	}
}

// 源集合变了要说出来（依据变了，结论就可能变了）。
func TestSourcesChangeIsVisible(t *testing.T) {
	rev := Revise(docOf("a"), docOf("a"), []string{"d1"}, []string{"d1", "d2"})
	if !rev.SourcesChanged {
		t.Fatal("sources changed must be reported")
	}
}

// 真跑发现的 bug ①：首次生成时**没有上一版**，不该说"内容与上一版一致"。
func TestFirstVersionHasNoComparisonNote(t *testing.T) {
	rev := Revise(nil, docOf("x"), nil, nil)
	if rev.Note() != "" {
		t.Fatalf("没有上一版就不该有差异说明: %q", rev.Note())
	}
}

// 真跑发现的 bug ②：同主题两次生成，模型措辞不同 → **字面比较会报假差异**
// （第一次真跑读到"新增 4 条、移除 5 条"，而内容其实没变多少）。
func TestReviseIgnoresWordingDrift(t *testing.T) {
	old := docOf("未在规定期限内缴纳年费的，可在六个月内补缴。", "发明专利年费三百八十元。")
	// 第二次生成：同一件事，换了措辞与标点
	new := docOf("未在规定期限内缴纳年费的可以在六个月内补缴", "发明专利年费是三百八十元")

	rev := Revise(old, new, nil, nil)
	if len(rev.Added) != 0 || len(rev.Removed) != 0 {
		t.Fatalf("措辞漂移不该算增删（真跑踩过：新增4/移除5的假差异）: +%v -%v", rev.Added, rev.Removed)
	}
	if rev.Note() != "内容与上一版一致（本次只是重新生成）" {
		t.Fatalf("note must be honest: %q", rev.Note())
	}
	// 但**真正新增的内容**仍要报出来（不能被"保守"吞掉）
	withNew := Revise(old, docOf("未在规定期限内缴纳年费的，可在六个月内补缴。", "新增：滞纳金为百分之五十"), nil, nil)
	if len(withNew.Added) != 1 {
		t.Fatalf("真新增必须报出来: %+v", withNew)
	}
}

// 真跑发现的 bug ③：模型把 `[1][2]` 引用标记写进 claim 文本 → 假差异。
// 比较前必须剥掉**纯数字**引用标记，但不能误伤 `[d1]` 那种源文档 id。
func TestReviseStripsCitationMarksButKeepsSourceIDs(t *testing.T) {
	base := "专利权人应当自专利权授予之日起每年缴纳年费"
	withMark := base + "[1][2]"
	rev := Revise(docOf(base), docOf(withMark), nil, nil)
	if len(rev.Added) != 0 || len(rev.Removed) != 0 {
		t.Fatalf("引用标记不许算成内容差异: +%v -%v", rev.Added, rev.Removed)
	}
	if got := stripCiteMarks("年费三百八十元 [d1]"); got != "年费三百八十元 [d1]" {
		t.Fatalf("源文档 id 不能被剥掉: %q", got)
	}
}

// 词面口径能处理**大多数**同义改写。下面三对都是真跑量到的真实差异，字面不同
// 但说的是同一件事（标点/顺序/引用标记/近义词级改写）。
//
// 词面**抓不住**的那一对（"请求减缓" vs "申请缓缴"）由语义判据负责，
// 见 TestReviseWithJudgeHandlesTrueSynonyms。
func TestReviseTreatsSynonymousRewritesAsSameClaim(t *testing.T) {
	same := [][2]string{
		{"实用新型年费为每一百九十元。", "实用新型年费为每一百九十元。"},
		{"专利代理服务按件收费，服务内容包括撰写权利要求书、答复审查意见等。", "专利代理服务按件收费，包含撰写权利要求书、答复审查意见等服务。"},
		{"委托代理事项应当签订书面委托合同，并明确保密义务。", "委托专利代理事项应当签订书面委托合同，并明确保密义务。"},
	}
	for _, pair := range same {
		if !sameClaim(pair[0], pair[1]) {
			t.Errorf("同义改写被当成不同论断了:\n  A=%s\n  B=%s", pair[0], pair[1])
		}
	}
	// 反面：**真的不同**必须判为不同（保守阈值不许把差异抹平）
	diff := [][2]string{
		{"发明专利年费三百八十元。", "专利代理服务按件收费。"},
		{"可在六个月内补缴年费。", "应当提交书面委托合同。"},
	}
	for _, pair := range diff {
		if sameClaim(pair[0], pair[1]) {
			t.Errorf("不相干的论断被当成同一条:\n  A=%s\n  B=%s", pair[0], pair[1])
		}
	}
}

// 语义判据：词面抓不住的同义改写（"请求减缓" vs "申请缓缴"）交给模型判；
// **判据缺席/失败一律算不同**（保守：宁可多报差异，不可谎报合并）。
func TestReviseWithJudgeHandlesTrueSynonyms(t *testing.T) {
	a := "请求减缓年费应当提交书面说明及证明材料，由专利局审查决定。"
	b := "申请缓缴年费时，应当提交书面说明并附证明材料，由专利局审查决定。"

	// 没有判据：词面抓不住 → 报差异（诚实）
	if rev := Revise(docOf(a), docOf(b), nil, nil); len(rev.Removed) != 1 {
		t.Fatalf("无判据时应诚实报差异: %+v", rev)
	}
	// 有判据且判"同一条" → 不报差异
	judge := func(x, y string) (bool, error) { return true, nil }
	if rev := ReviseWith(docOf(a), docOf(b), nil, nil, judge); len(rev.Added) != 0 || len(rev.Removed) != 0 {
		t.Fatalf("判据判同一条时不该报差异: %+v", rev)
	}
	// 判据判"不同" → 照实报差异
	noJudge := func(x, y string) (bool, error) { return false, nil }
	if rev := ReviseWith(docOf(a), docOf(b), nil, nil, noJudge); len(rev.Removed) != 1 {
		t.Fatalf("判据判不同就该报差异: %+v", rev)
	}
	// 判据失败 → 视为不同（保守方向）
	errJudge := func(x, y string) (bool, error) { return false, fmt.Errorf("down") }
	if rev := ReviseWith(docOf(a), docOf(b), nil, nil, errJudge); len(rev.Removed) != 1 {
		t.Fatalf("判据失败必须视为不同: %+v", rev)
	}
}
