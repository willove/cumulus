package facts

import (
	"strings"
	"testing"
)

func TestJaccardBounds(t *testing.T) {
	if Jaccard("连接池最大连接数", "连接池最大连接数") != 1 {
		t.Fatal("identical → 1")
	}
	p := Jaccard("连接池最大连接数", "部署机房端口")
	if p > 0.2 {
		t.Fatalf("unrelated should be near 0, got %v", p)
	}
	if Jaccard("", "x") != 0 {
		t.Fatal("empty side → 0")
	}
}

func TestFilterDissimilarDropsNearCopies(t *testing.T) {
	origin := "网购七天无理由退货怎么退"
	tried := []string{"七天无理由退货流程"}
	cands := []string{
		"七天无理由退货流程",
		"网购七天无理由退货怎么退",
		"消费者后悔权行使期限",
	}
	got := FilterDissimilar(origin, tried, cands, 0.5)
	if len(got) != 1 || !strings.Contains(got[0], "后悔权") {
		t.Fatalf("got %v", got)
	}
}
