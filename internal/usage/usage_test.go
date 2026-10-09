package usage

import (
	"strings"
	"testing"
	"time"
)

func newFixedMeter(q map[string]Quota) (*Meter, func(d time.Duration)) {
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	m := NewMeter(q)
	m.now = func() time.Time { return now }
	m.resetLocked()
	return m, func(d time.Duration) { now = now.Add(d) }
}

// 超限要拦、且**超限的那次不计数**（否则一次拒绝会让剩余额度更快耗尽）。
func TestQuotaBlocksAndDoesNotCountRejectedCall(t *testing.T) {
	m, _ := newFixedMeter(map[string]Quota{"alpha": {QuestionsPerDay: 2}})
	for i := 0; i < 2; i++ {
		if over, _ := m.Exceeded("alpha", KindQuestion); over {
			t.Fatalf("第 %d 次不该超限", i+1)
		}
		m.Record("alpha", KindQuestion)
	}
	over, why := m.Exceeded("alpha", KindQuestion)
	if !over {
		t.Fatal("第 3 次应超限")
	}
	for _, want := range []string{"alpha", "问答次数", "2", "重置"} {
		if !strings.Contains(why, want) {
			t.Fatalf("超限原因要可解释（缺 %q）: %s", want, why)
		}
	}
	if got := m.Snapshot("alpha").Usage.Questions; got != 2 {
		t.Fatalf("超限的那次不许被计数: %d", got)
	}
}

// realm 之间互不影响（配额不是全局的）。
func TestQuotaIsPerRealm(t *testing.T) {
	m, _ := newFixedMeter(map[string]Quota{"alpha": {QuestionsPerDay: 1}})
	m.Record("alpha", KindQuestion)
	if over, _ := m.Exceeded("alpha", KindQuestion); !over {
		t.Fatal("alpha 应超限")
	}
	if over, _ := m.Exceeded("beta", KindQuestion); over {
		t.Fatal("beta 不该受 alpha 的配额影响")
	}
}

// 无配额配置的 realm = 不限（缺席不是拒绝）。
func TestNoQuotaMeansUnbounded(t *testing.T) {
	m, _ := newFixedMeter(map[string]Quota{"alpha": {QuestionsPerDay: 1}})
	for i := 0; i < 50; i++ {
		if over, _ := m.Exceeded("gamma", KindQuestion); over {
			t.Fatal("未配额的 realm 必须不限")
		}
		m.Record("gamma", KindQuestion)
	}
}

// 跨日自动清零（配额是"每天"的，不是永久总量）。
func TestQuotaRollsOverDaily(t *testing.T) {
	m, advance := newFixedMeter(map[string]Quota{"alpha": {QuestionsPerDay: 1}})
	m.Record("alpha", KindQuestion)
	if over, _ := m.Exceeded("alpha", KindQuestion); !over {
		t.Fatal("当日第二次应超限")
	}
	advance(25 * time.Hour)
	if over, _ := m.Exceeded("alpha", KindQuestion); over {
		t.Fatal("跨日后配额应重置")
	}
	snap := m.Snapshot("alpha")
	if snap.Usage.Questions != 0 {
		t.Fatalf("跨日应清零: %+v", snap.Usage)
	}
	if snap.ResetsAt == "" {
		t.Fatal("读数要给出重置时刻（客户端才知道该等多久）")
	}
}

// token 不知道就说不知道（**不许填 0 冒充没用钱**）。
func TestTokensHonesty(t *testing.T) {
	m, _ := newFixedMeter(nil)
	m.RecordTokens("alpha", 0, 0, false)
	snap := m.Snapshot("alpha")
	if snap.Usage.TokensKnown {
		t.Fatal("provider 没给 usage 时 TokensKnown 必须为 false")
	}
	if snap.Usage.TokensIn != 0 || snap.Usage.TokensOut != 0 {
		t.Fatalf("未知时不该有数: %+v", snap.Usage)
	}
	m.RecordTokens("alpha", 120, 45, true)
	snap = m.Snapshot("alpha")
	if !snap.Usage.TokensKnown || snap.Usage.TokensIn != 120 || snap.Usage.TokensOut != 45 {
		t.Fatalf("已知时要如实记: %+v", snap.Usage)
	}
	// 已知之后仍然收到 unknown → 不该把已知抹掉
	m.RecordTokens("alpha", 0, 0, false)
	if !m.Snapshot("alpha").Usage.TokensKnown {
		t.Fatal("后来的 unknown 不该抹掉已知的计数")
	}
}

// 读数自解释：超限的维度直接标出来（不用去比数字）。
func TestSnapshotFlagsOverQuota(t *testing.T) {
	m, _ := newFixedMeter(map[string]Quota{"alpha": {QuestionsPerDay: 1, DocsPerDay: 1}})
	m.Record("alpha", KindQuestion)
	m.Record("alpha", KindDoc)
	snap := m.Snapshot("alpha")
	if !snap.Over["questions"] || !snap.Over["docs"] {
		t.Fatalf("超限维度必须标出: %+v", snap.Over)
	}
	all := m.All()
	if len(all) == 0 || all[0].Realm != "alpha" {
		t.Fatalf("All 应按 realm 排序返回: %+v", all)
	}
}
