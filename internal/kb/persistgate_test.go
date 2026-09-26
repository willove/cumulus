package kb

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// The persist gate used to judge the answer's own SUMMARY against its own
// query — a self-confirmation loop (audit C2): any pipeline that echoed the
// query's words in the summary passed the gate by construction, and that
// same summary became the cluster's content and the reuse surface. The
// gate now judges the pinned EVIDENCE: a summary that echoes "闯红灯" over
// windows that never mention it must NOT persist.
func TestPersistGateJudgesEvidenceNotSummary(t *testing.T) {
	ctx := context.Background()
	src := source.New("A", "txt", "", "a", "zh", "行政处罚法规定处罚的种类与程序。", nil)
	corpus := []source.Source{src}

	t.Run("summary echoes query, evidence does not", func(t *testing.T) {
		e := New(nil, cluster.NewMemory(), cluster.Local{N: 64})
		ans := fast.Answer{
			Query: "闯红灯会有什么处罚", Summary: "闯红灯的处罚：闯红灯会被处罚。",
			SourceID: src.ID, Confidence: 0.8,
			// The window is real corpus text but contains none of the query's terms.
			Samples: []mcs.Sample{{Source: src.ID, Start: 0, End: 20, Content: src.Body, Score: 8}},
		}
		r, err := e.Persist(ctx, ans, corpus)
		if err != nil {
			t.Fatal(err)
		}
		if r.Persisted {
			t.Fatal("an answer whose evidence never touches the query must not persist — the gate was reading the summary again")
		}
	})
	t.Run("evidence carries the query terms", func(t *testing.T) {
		e := New(nil, cluster.NewMemory(), cluster.Local{N: 64})
		ans := fast.Answer{
			Query: "闯红灯会有什么处罚", Summary: "闯红灯的处罚见道交法。",
			SourceID: src.ID, Confidence: 0.8,
			Samples: []mcs.Sample{{Source: src.ID, Start: 0, End: 20, Content: "闯红灯的，处二百元罚款。", Score: 8}},
		}
		r, err := e.Persist(ctx, ans, corpus)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Persisted {
			t.Fatal("an answer grounded in query-touching evidence must persist")
		}
	})
}
