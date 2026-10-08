package evaldata

import (
	"testing"

	"github.com/willove/cumulus/internal/evalfcore"
)

func itemsOf(ids ...string) []evalfcore.Item {
	out := make([]evalfcore.Item, 0, len(ids))
	for _, id := range ids {
		out = append(out, evalfcore.Item{ID: id, Question: "问题" + id, Answer: "答", GoldIDs: []string{"d1"}})
	}
	return out
}

// 切分必须与输入顺序无关、且互斥完备——锁箱纪律的前提是"这一题永远是
// 锁箱题"，顺序一变就滑进校准集的话，锁箱就不存在了。
func TestSplitItemsIsOrderIndependentAndComplete(t *testing.T) {
	ids := []string{"q1", "q2", "q3", "q4", "q5", "q6", "q7", "q8", "q9", "q10", "q11", "q12"}
	a := SplitItems(itemsOf(ids...), 0.5, 0.25)
	rev := make([]string, len(ids))
	for i, id := range ids {
		rev[len(ids)-1-i] = id
	}
	b := SplitItems(itemsOf(rev...), 0.5, 0.25)

	where := func(parts map[SplitName][]evalfcore.Item) map[string]SplitName {
		m := map[string]SplitName{}
		for name, its := range parts {
			for _, it := range its {
				if prev, dup := m[it.ID]; dup {
					t.Fatalf("item %s in two splits: %s and %s", it.ID, prev, name)
				}
				m[it.ID] = name
			}
		}
		return m
	}
	ma, mb := where(a), where(b)
	if len(ma) != len(ids) {
		t.Fatalf("splits must cover every item: %d/%d", len(ma), len(ids))
	}
	for id, wa := range ma {
		if mb[id] != wa {
			t.Fatalf("split assignment must not depend on order: %s %s vs %s", id, wa, mb[id])
		}
	}
	// 三份都不空（12 题按 50/25/25 分）
	if len(a[SplitCalib]) == 0 || len(a[SplitVal]) == 0 || len(a[SplitLockbox]) == 0 {
		t.Fatalf("all three splits must be non-empty: %d/%d/%d",
			len(a[SplitCalib]), len(a[SplitVal]), len(a[SplitLockbox]))
	}
}

// 子集指纹：同一语料 + 不同题集 ⇒ ItemsSHA 不同（内容寻址是锁箱可区分
// 的机械保证，不靠文件名）。
func TestSplitSubsetsHaveDistinctFingerprints(t *testing.T) {
	parts := SplitItems(itemsOf("q1", "q2", "q3", "q4", "q5", "q6", "q7", "q8"), 0.5, 0.25)
	seen := map[string]SplitName{}
	for _, name := range []SplitName{SplitCalib, SplitVal, SplitLockbox} {
		ds, err := evalfcore.NewDataset(parts[name])
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[ds.ItemsSHA]; dup {
			t.Fatalf("split %s and %s share a fingerprint %s", prev, name, ds.ItemsSHA)
		}
		seen[ds.ItemsSHA] = name
	}
}

// 增长不搬家：往题集里加题，已有题的归属不变（校准集可以长大而不污染锁箱）。
func TestSplitAssignmentsSurviveGrowth(t *testing.T) {
	base := SplitItems(itemsOf("q1", "q2", "q3", "q4", "q5", "q6"), 0.5, 0.25)
	grown := SplitItems(itemsOf("q1", "q2", "q3", "q4", "q5", "q6", "q7", "q8"), 0.5, 0.25)
	where := func(parts map[SplitName][]evalfcore.Item) map[string]SplitName {
		m := map[string]SplitName{}
		for name, its := range parts {
			for _, it := range its {
				m[it.ID] = name
			}
		}
		return m
	}
	a, b := where(base), where(grown)
	for id, wa := range a {
		if b[id] != wa {
			t.Fatalf("adding items must not move existing ones: %s %s → %s", id, wa, b[id])
		}
	}
}
