package prior

import (
	"testing"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// 历史成功证据: a source whose evidence hits were marked live must
// outrank one with no history, other signals equal.
func TestHistoryArmLiftsPreviouslySuccessfulSource(t *testing.T) {
	// Distinct bodies: source IDs are content-addressed, so identical bodies
	// collapse to one ID and both sides would match history.
	a := source.New("手册A", "md", "", "a", "zh", "连接池最大 128，超时 30 秒。", nil)
	b := source.New("手册B", "md", "", "b", "zh", "连接池最大 128，超时 30 秒。补充说明。", nil)
	if a.ID == b.ID {
		t.Fatal("fixture must have distinct ids")
	}
	fields := mcs.Fields("连接池")

	hist := HistoryFrom([]string{a.ID}, []string{"连接池最大 128"})

	belNo := Rank(fields, []source.Source{a, b}, nil, 10)
	belHi := Rank(fields, []source.Source{a, b}, hist, 10)

	sa := scoreOf(belHi, a.ID)
	sb := scoreOf(belHi, b.ID)
	na := scoreOf(belNo, a.ID)
	nb := scoreOf(belNo, b.ID)
	if sa <= sb {
		t.Fatalf("history arm must lift a: hi=%.3f vs b=%.3f (no-hist a=%.3f)", sa, sb, na)
	}
	// Rank normalizes the top file to 1, so compare the *gap*: history must
	// widen a's lead over b.
	if (sa - sb) <= (na - nb) {
		t.Fatalf("history arm must widen the lead: gap %.3f → %.3f", na-nb, sa-sb)
	}
	// Normalization: top file rescales to 1.
	if belHi.Files[0].Score != 1 {
		t.Fatalf("top file must normalize to 1: %+v", belHi.Files[0])
	}
}

func scoreOf(b Belief, id string) float64 {
	for _, f := range b.Files {
		if f.SourceID == id {
			return f.Score
		}
	}
	return 0
}
