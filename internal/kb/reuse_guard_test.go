package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// The cross-mode poisoning this file pins (2026-09-28, reproduced live): an
// offline CLI search (no endpoint → no synthesizer) persists template-shaped
// clusters by design, and an online stack then served one back as a reused
// answer for 0 tokens — the question replayed as knowledge. The read boundary
// mirrors the persist boundary: with a synthesizer wired, template content is
// not reusable; without one (the offline gates), reuse is untouched.
//
// The evidence windows below are exact slices of the fixture body, because
// priorStale pins every stored window back to the corpus — a fabricated window
// would read as a stale prior and fall to L0 for a reason that has nothing to
// do with the guard under test.

// templateAnswerWithRealWindow is the production shape: real, on-topic,
// exactly-pinning evidence — and a deterministic template as the summary.
func templateAnswerWithRealWindow(query string, src source.Source) fast.Answer {
	needle := "关键配置：连接池最大 128，超时 30 秒。"
	byteIdx := strings.Index(src.Body, needle)
	start := len([]rune(src.Body[:byteIdx]))
	end := start + len([]rune(needle))
	sm := mcs.Sample{
		Start:   start,
		End:     end,
		Content: needle,
		Source:  src.ID,
		Score:   8,
	}
	return fast.Answer{
		Query:      query,
		Mode:       fast.ModeFAST,
		SourceID:   src.ID,
		Samples:    []mcs.Sample{sm},
		Coverage:   0.9,
		Confidence: 0.85,
		Summary:    synthesizeTemplate(query, src, []mcs.Sample{sm}),
	}
}

// TestOnlineReuseSkipsTemplateCluster is the live sequence in miniature: an
// offline persist plants the template cluster, then an online engine asks the
// same query and must NOT reuse it.
func TestOnlineReuseSkipsTemplateCluster(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	q := "连接池最大连接数是多少"
	srcs := fixtureSources()

	// Offline half: no synth wired, template persists (pinned by
	// TestOfflineGatesStillPersistTemplate).
	off := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64})
	pr, err := off.Persist(ctx, templateAnswerWithRealWindow(q, srcs[0]), srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Persisted {
		t.Fatal("fixture sanity: the offline template answer must persist")
	}

	// Online half: synthesizer present. The template cluster must be invisible
	// to reuse — the query falls to L0 and answers with the synth text.
	on := withSynth(New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64}), "连接池最大 128，超时 30 秒。")
	res, err := on.Ask(ctx, q, srcs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reused {
		t.Fatal("an online stack replayed a template cluster — the question " +
			"would be served as knowledge for 0 tokens")
	}
	if res.Answer.Summary != "连接池最大 128，超时 30 秒。" {
		t.Fatalf("the fresh answer must be the synthesizer's, got %q", res.Answer.Summary)
	}
}

// TestOfflineReuseStillServesTemplate is the regression guard for the guard:
// the offline gates exercise reuse against template-shaped clusters (e2e Gate
// B asserts reused=true under CLUS_OFFLINE), so the skip must key on a wired
// synthesizer, not on the template shape alone.
func TestOfflineReuseStillServesTemplate(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	q := "连接池最大连接数是多少"
	srcs := fixtureSources()

	off := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64})
	if _, err := off.Persist(ctx, templateAnswerWithRealWindow(q, srcs[0]), srcs); err != nil {
		t.Fatal(err)
	}
	res, err := off.Ask(ctx, q, srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reused {
		t.Fatal("offline-to-offline reuse of a template cluster must keep working — " +
			"the deterministic gates depend on it")
	}
	if !fast.TemplateDegraded(res.Answer.Summary) {
		t.Fatalf("expected the stored template back, got %q", res.Answer.Summary)
	}
}

// A deferred answer must not persist: its summary is an unpaid debt, and a
// cluster whose content is empty is exactly the template-replay class of
// poison the persistence boundary exists to stop.
func TestSaveAnswerSkipsDeferredSynth(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := withSynth(New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64}), "连接池最大 128，超时 30 秒。")
	srcs := fixtureSources()
	ans := templateAnswerWithRealWindow("连接池最大连接数是多少", srcs[0])
	ans.SynthDeferred = true
	ans.Summary = ""
	res, err := e.Persist(ctx, ans, srcs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Persisted {
		t.Fatal("a deferred answer (empty summary) was persisted as cluster content")
	}
}
