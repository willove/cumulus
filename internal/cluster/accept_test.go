package cluster

import (
	"fmt"
	"testing"

	"github.com/willove/cumulus/internal/mcs"
)

// A2: Specificity — a proposed key that would not retrieve the winner must
// refuse the fold (unvalidated evolution is net-negative).
func TestAcceptFoldSpecificityRejectsForeignKey(t *testing.T) {
	winner := Cluster{ID: "W", TopicKey: "连接池最大连接数", Content: "连接池最大是 128。", Queries: []string{"连接池最大连接数是多少"}}
	loser := Cluster{ID: "L", TopicKey: "照明功率", Content: "照明 200 瓦。", Queries: []string{"照明功率是多少"}}
	// Competitor actually owns the loser's key material.
	comp := Cluster{ID: "C", TopicKey: "照明功率", Content: "照明 200 瓦 功率。", Queries: []string{"照明功率是多少"}}
	if ok, why := AcceptFold(winner, loser, []Cluster{winner, loser, comp}, 1); ok {
		t.Fatalf("foreign key must fail specificity: %s", why)
	}
}

// A2: same-domain paraphrase must still fold (tidy's raison d'être).
func TestAcceptFoldAllowsSameDomainParaphrase(t *testing.T) {
	winner := Cluster{ID: "W", TopicKey: "连接池最大连接数是多少", Content: "连接池最大 128。", Queries: []string{"连接池最大连接数是多少"}}
	loser := Cluster{ID: "L", TopicKey: "连接池最大连接数上限是多少", Content: "连接池上限 128。", Queries: []string{"连接池最大连接数上限是多少"}}
	ok, why := AcceptFold(winner, loser, []Cluster{winner, loser}, 3)
	if !ok {
		t.Fatalf("same-domain paraphrase must pass AcceptFold: %s", why)
	}
}

// A2: Separation — proposed key closer to a competitor than to winner is refused.
func TestAcceptFoldSeparationRejectsCompetitorOwnedKey(t *testing.T) {
	winner := Cluster{ID: "W", TopicKey: "数据库连接池", Content: "连接池 配置 上限。", Queries: []string{"数据库连接池怎么配"}}
	loser := Cluster{ID: "L", TopicKey: "缓存穿透雪崩", Content: "缓存穿透 雪崩 熔断。", Queries: []string{"缓存穿透怎么处理"}}
	comp := Cluster{ID: "C", TopicKey: "缓存穿透雪崩", Content: "缓存穿透 雪崩 熔断 降级。", Queries: []string{"缓存穿透怎么处理"}}
	ok, _ := AcceptFold(winner, loser, []Cluster{winner, loser, comp}, 1)
	if ok {
		t.Fatal("key owned by competitor must fail separation/specificity")
	}
}

// Divergent numeric claims on a cross-topic near-hit are conflicts, not
// duplicates (128↔256, 96↔192). Same-topic folds still allow
// a claim replacement so self-heal can update a stale number.
func TestAcceptFoldRejectsDivergentCrossTopicClaims(t *testing.T) {
	winner := Cluster{
		ID: "W", TopicKey: "k128", Content: "连接池最大 128。",
		Queries:  []string{"连接池最大连接数"},
		Evidence: []mcs.Sample{{Content: "关键配置：连接池最大 128，超时 30 秒。"}},
	}
	loser := Cluster{
		ID: "L", TopicKey: "k256", Content: "连接池参数已改为 256。",
		Queries: []string{"连接池最大连接数 128"},
	}
	if ok, why := AcceptFold(winner, loser, []Cluster{winner, loser}, 3); ok {
		t.Fatalf("cross-topic divergent claims 128 vs 256 must refuse fold: %s", why)
	}
	// Same topic key: a claim update must still pass AcceptFold.
	loser.TopicKey = winner.TopicKey
	ok, why := AcceptFold(winner, loser, []Cluster{winner, loser}, 3)
	if !ok {
		t.Fatalf("same-topic claim update must still fold: %s", why)
	}
}

// Production TopicKeys are sha256 identities, not text: as a "proposed key"
// a hash matches nothing (keyRel = 0 against every cluster), so the
// specificity gate fell back to ID-order — with more than topK clusters
// alive, a same-domain fold was rejected on ID sort order alone. This
// reproduces that shape (unreadable identities, winner ID sorting LAST,
// five unrelated clusters) and pins that the fold is decided on the query
// text, which is the only thing a future search can issue.
func TestAcceptFoldIgnoresTopicKeyHashIdentity(t *testing.T) {
	winner := Cluster{ID: "W9", TopicKey: "99ea9360f3babe85", Content: "红灯亮时，禁止车辆通行。", Queries: []string{"闯红灯会有什么处罚"}}
	loser := Cluster{ID: "L8", TopicKey: "48f360fb70b0ae36", Content: "违反交通信号灯通行的处罚。", Queries: []string{"闯红灯怎么处罚"}}
	corpus := []Cluster{winner, loser}
	for i := 0; i < 5; i++ {
		corpus = append(corpus, Cluster{
			ID: fmt.Sprintf("A%d", i), TopicKey: fmt.Sprintf("hash%016d", i),
			Content: fmt.Sprintf("无关文档 %d 的内容。", i), Queries: []string{fmt.Sprintf("无关的问题 %d", i)},
		})
	}
	ok, why := AcceptFold(winner, loser, corpus, 3)
	if !ok {
		t.Fatalf("same-domain fold with hash identities must be decided on query text, not ID order: %s", why)
	}
}
