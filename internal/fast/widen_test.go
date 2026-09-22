package fast

import (
	"context"

	"github.com/cumubase/ask/internal/mcs"
	"testing"

	"github.com/cumubase/ask/internal/source"
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
	got, err := e.WidenSources(context.Background(), "网购的商品七天无理由退货有法律依据吗？", sources, map[string]bool{}, 4)
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
	got2, _ := e.WidenSources(context.Background(), "网购的商品七天无理由退货有法律依据吗？", sources, map[string]bool{got[0].ID: true}, 4)
	if len(got2) != 0 {
		t.Fatalf("exclude not honored: %d", len(got2))
	}
}
