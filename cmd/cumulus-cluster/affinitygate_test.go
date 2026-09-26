package main

import (
	"context"
	"testing"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/affinity"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/ns"
)

// The ledger and the session stack used to record EVERY finished answer,
// refusals included — so a high-lexical-coverage refusal (the red-light
// case: a confident-sounding gap report with conf≥0.7) taught the ledger
// the WRONG document at full OutcomeWeight, and the session stack (which
// feeds the default-on ranking path) with it. kb.Persist and MarkEvidence
// both gate on Refused/Skipped; this path now does too, and the control
// half pins that a healthy answer still teaches.
func TestRecordUsageSkipsRefusedAndSkippedAnswers(t *testing.T) {
	c, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	// The gate test writes the ledger; declare it like serve does at boot.
	if err := c.EnsureCollection(ctx, ns.Coll("alpha", "clus_affinity")); err != nil {
		t.Fatal(err)
	}
	refs := []deep.Ref{{SourceID: "src:中华人民共和国行政处罚法.txt#2"}}

	// NOTE: the three queries must share NO bigram — the ledger is indexed by
	// query tokens, so a shared fragment ("红灯") would let one query's
	// teaching surface under another's lookup (exactly the rich-get-richer
	// dynamic X3 describes). Token-disjoint queries keep the assertion about
	// the GATE, not about token overlap.
	good := fast.Answer{
		Query: "连接池最大连接数是多少", Summary: "上限为 128。",
		SourceID: "src:部署手册#2", Confidence: 0.62,
	}
	recordUsage(ctx, c, "alpha", good.Query, "gate-good", good, refs)

	// Refusal WITH citations and a high confidence: the exact red-light shape.
	refused := fast.Answer{
		Query: "闯红灯会有什么处罚", Summary: "证据不足，暂不作答。",
		SourceID: "src:中华人民共和国行政处罚法.txt#2", Confidence: 0.75, Refused: true,
	}
	recordUsage(ctx, c, "alpha", refused.Query, "gate-refused", refused, refs)

	skipped := fast.Answer{Query: "另一个问题", Skipped: true, Confidence: 0.8, SourceID: "src:doc#2"}
	recordUsage(ctx, c, "alpha", skipped.Query, "gate-skipped", skipped, refs)

	led := affinity.NewCumuStore(c, ns.Coll("alpha", "clus_affinity"))
	for _, q := range []string{refused.Query, skipped.Query} {
		w, err := led.Weights(ctx, queryTokens(q), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for id := range w {
			t.Fatalf("query %q taught the ledger %q — a refusal must leave no trace", q, id)
		}
	}
	w, err := led.Weights(ctx, queryTokens(good.Query), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(w) == 0 {
		t.Fatal("control failed: a healthy answer taught the ledger nothing")
	}
	if got := sessEvidence.Weights("alpha", "gate-refused", time.Now()); len(got) != 0 {
		t.Fatalf("refused session taught the session stack: %v", got)
	}
	if got := sessEvidence.Weights("alpha", "gate-skipped", time.Now()); len(got) != 0 {
		t.Fatalf("skipped session taught the session stack: %v", got)
	}
	if got := sessEvidence.Weights("alpha", "gate-good", time.Now()); len(got) == 0 {
		t.Fatal("control failed: a healthy session taught the stack nothing")
	}
}
