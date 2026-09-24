package fast

import (
	"context"

	"github.com/willove/cumulus/internal/mcs"
	"testing"

	"github.com/willove/cumulus/internal/source"
)

func TestWidenSourcesOffline(t *testing.T) {
	mk := func(key, text string) source.Source {
		return source.New("t:"+key, "md", "", key, "zh", text, nil)
	}
	sources := []source.Source{
		mk("a", "《消费者权益保护法》第二十五条：经营者采取网络方式销售商品，消费者有权七日内退货，且无需说明理由。"),
		mk("b", "《海商法》第八十八条：海上货物运输合同……"),
		mk("c", "《民法典》第六百零九条：出卖人交付标的物……"),
	}
	e := New(mcs.KeywordScorer{})
	got, _, err := e.WidenSources(context.Background(), "网购的商品七天无理由退货有法律依据吗？", sources, map[string]bool{}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("widen admitted 0 sources offline")
	}
	if got[0].BusinessKey != "a" {
		t.Fatalf("top widen = %s, want a", got[0].BusinessKey)
	}

	// exclude must filter.
	got2, _, _ := e.WidenSources(context.Background(), "网购的商品七天无理由退货有法律依据吗？", sources, map[string]bool{got[0].ID: true}, 4)
	if len(got2) != 0 {
		t.Fatalf("exclude not honored: %d", len(got2))
	}
}

func TestWidenSourcesRealizationProbe(t *testing.T) {
	e := New(mcs.KeywordScorer{})
	srcs := []source.Source{
		source.New("反家庭暴力法·第一条", "md", "", "a", "zh", "《中华人民共和国反家庭暴力法》第一条规定，为了预防和制止家庭暴力，保护家庭成员的合法权益，维护平等、和睦、文明的家庭关系，促进家庭和谐、社会稳定，制定本法。", nil),
		source.New("反家庭暴力法·第二条", "md", "", "b", "zh", "《中华人民共和国反家庭暴力法》第二条规定，本法所称家庭暴力，是指家庭成员之间以殴打、捆绑、残害、限制人身自由以及经常性谩骂、恐吓等方式实施的身体、精神等侵害行为。", nil),
	}
	for _, q := range []string{
		"国家为什么专门针对家里打人的事立个法？",
		"经常性谩骂和恐吓算不算家庭暴力？",
		"家庭暴力",
	} {
		got, _, err := e.WidenSources(context.Background(), q, srcs, map[string]bool{}, 4)
		t.Logf("%q -> admitted=%d err=%v", q, len(got), err)
		for _, s := range got {
			t.Logf("   %s %s", s.BusinessKey, s.Title)
		}
	}
}
