package evalfcore

import "testing"

// "该不该检索"这一维：标注了才统计，未标注（nil）不参与——外部语料上这维
// 全是 nil（那些题几乎都该检索），所以它只在**自有语料**上有读数。
func TestShouldRetrieveDimension(t *testing.T) {
	yes, no := true, false
	st := RunState{}
	st.Results = []ItemResult{
		{ItemID: "a", ShouldRetrieve: &yes, RetrievedAnyway: true, EvidenceHit: true},
		{ItemID: "b", ShouldRetrieve: &no, RetrievedAnyway: true}, // 不该检索却检索了
		{ItemID: "c", RetrievedAnyway: true},                      // 未标注
	}
	got := Summarize(st)
	if got.SRAnnotated != 2 {
		t.Fatalf("only annotated items count: %+v", got)
	}
	if got.SRShouldNoRet != 1 || got.SRRetrieved != 1 {
		t.Fatalf("should-not-retrieve wrong: %+v", got)
	}
	if got.SRShouldRetMiss != 0 {
		t.Fatalf("should-retrieve items were retrieved: %+v", got)
	}

	// 该检索却没检索（闸门/路由拦过头）也要能读出来
	st.Results[0] = ItemResult{ItemID: "a", ShouldRetrieve: &yes, RetrievedAnyway: false}
	got = Summarize(st)
	if got.SRShouldRetMiss != 1 {
		t.Fatalf("must count over-refusal: %+v", got)
	}
}
