package qaflow

import (
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/retrieval"
)

func testIdx() *retrieval.Index {
	return retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。"},
	})
}

// 漂移闸：改写保留原问的语料内内容词 → 采纳（检索用改写）。
func TestDriftGateAcceptsFaithfulRewrite(t *testing.T) {
	idx := testIdx()
	c := context.New("default")
	st := RewriteStage{Query: "连接池上限是多少", Hypothetical: "系统里的连接池配置项上限设为多少", Idx: idx}
	if err := st.Run(c); err != nil {
		t.Fatal(err)
	}
	rw, _ := context.Get(c, KeyRewrite)
	if rw.DriftRejected {
		t.Fatalf("faithful rewrite must pass: %+v", rw)
	}
	if rw.Effective() != "系统里的连接池配置项上限设为多少" {
		t.Fatalf("retrieval must use the accepted rewrite, got %q", rw.Effective())
	}
	// 覆盖度/接地仍按原问——改写管召回，原问管接地
	if rw.Original != "连接池上限是多少" {
		t.Fatal("original must be preserved for grounding")
	}
}

// 漂移闸：改写丢掉原问的内容词（“连接池”没了）→ 拦下，退回原问，
// 丢词清单入账。
func TestDriftGateRejectsDriftedRewrite(t *testing.T) {
	idx := testIdx()
	c := context.New("default")
	st := RewriteStage{Query: "连接池上限是多少", Hypothetical: "数据库连接相关参数如何调整", Idx: idx}
	if err := st.Run(c); err != nil {
		t.Fatal(err)
	}
	rw, _ := context.Get(c, KeyRewrite)
	if !rw.DriftRejected {
		t.Fatal("drifted rewrite must be rejected")
	}
	if len(rw.DroppedTerms) == 0 {
		t.Fatal("dropped terms must be recorded")
	}
	if rw.Effective() != "连接池上限是多少" {
		t.Fatalf("retrieval must fall back to the original, got %q", rw.Effective())
	}
}

// 没有索引可判语料内/外：闸不启动（放行并记账语义由 Effective 兜底——
// 无改写时永远原问）。
func TestDriftGateWithoutIndexSkips(t *testing.T) {
	c := context.New("default")
	st := RewriteStage{Query: "连接池上限是多少", Hypothetical: "完全不相干的一句话"}
	if err := st.Run(c); err != nil {
		t.Fatal(err)
	}
	rw, _ := context.Get(c, KeyRewrite)
	if rw.DriftRejected {
		t.Fatal("gate without index must not reject")
	}
}

// 不改写（Hypothetical 空）：Effective 就是原问。
func TestEffectiveWithoutRewrite(t *testing.T) {
	rw := Rewrite{Original: "原问"}
	if rw.Effective() != "原问" {
		t.Fatal("no rewrite means original")
	}
}
