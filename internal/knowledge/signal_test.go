package knowledge

import (
	"os"
	"path/filepath"
	"testing"
)

// 再问族的第一形态：同 session 同问句第二次出现才算。首问不触发。
func TestRememberFiresOnlyOnReask(t *testing.T) {
	st := NewSignalStore("")
	if sig := st.Remember("s1", "闯红灯怎么处罚", false); sig != nil {
		t.Fatalf("首问不该触发信号：%+v", sig)
	}
	if sig := st.Remember("s1", "闯红灯怎么处罚", false); sig == nil {
		t.Fatal("原样再问必须触发")
	} else if sig.Kind != SignalReaskAfterAnswer {
		t.Fatalf("答过又原样问 = 答案没答全：%+v", sig)
	}
	if sig := st.Remember("s1", "换个问法", true); sig != nil {
		t.Fatalf("换了问法不是再问（原样匹配）：%+v", sig)
	}
}

// 拒答后又原样问：弃权没解决用户的问题——与"答了又原样问"分族。
func TestRememberSplitsReaskByRefusal(t *testing.T) {
	st := NewSignalStore("")
	st.Remember("s2", "红灯怎么处罚", true) // 首问被拒答
	sig := st.Remember("s2", "红灯怎么处罚", false)
	if sig == nil || sig.Kind != SignalReaskAfterRefusal {
		t.Fatalf("拒答后原样再问 = 弃权没解决：%+v", sig)
	}
}

// 无 session 的一次性问答不记：没有"再问"的上下文。
func TestRememberSkipsEphemeral(t *testing.T) {
	st := NewSignalStore("")
	st.Remember("", "q", false)
	if sig := st.Remember("", "q", false); sig != nil {
		t.Fatalf("无 session 不记：%+v", sig)
	}
	if st.Len() != 0 {
		t.Fatalf("无 session 一条都不该有：%d", st.Len())
	}
}

// 归一化：大小写/空白不同的同一问句共享再问判定。
func TestRememberNormalizes(t *testing.T) {
	st := NewSignalStore("")
	st.Remember("s3", "Patent Term", false)
	if sig := st.Remember("s3", "  patent term ", false); sig == nil {
		t.Fatal("归一化后同一问句第二次该触发")
	}
}

// 落盘往返：重启后信号与 session 状态都在（信号是资产，跨重启要活着）。
func TestSignalStorePersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "signals.json")
	st := NewSignalStore(path)
	st.Remember("s4", "q1", false)
	st.Remember("s4", "q1", false) // 触发一次
	st.Record(Signal{Session: "s4", Kind: SignalCitationClick, Target: "doc1#rune[0:10]"})

	reopened := NewSignalStore(path)
	if reopened.Len() != 2 { // 一次再问 + 一次引用点击
		t.Fatalf("重开少了两条：%d", reopened.Len())
	}
	// session 状态也要在：再问第三次还触发
	if sig := reopened.Remember("s4", "q1", false); sig == nil {
		t.Fatal("重开后同 session 同问句第三次还该触发")
	}
	counts := reopened.Counts()
	if counts[SignalReaskAfterAnswer]+counts[SignalReaskAfterRefusal] != 2 {
		t.Fatalf("计数不对：%v", counts)
	}
	if counts[SignalCitationClick] != 1 {
		t.Fatalf("引用点击也必须在：%v", counts)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("文件不在：%v", err)
	}
}

// 聚合：top 问句按计数降序（靶子表要能排序）。
func TestTopQuestionsOrdered(t *testing.T) {
	st := NewSignalStore("")
	for _, q := range []string{"a", "b", "b", "c", "c", "c"} {
		st.Record(Signal{Kind: SignalReaskAfterAnswer, Question: q})
	}
	top := st.TopQuestions(SignalReaskAfterAnswer, 2)
	if len(top) != 2 || top[0].Question != "c" || top[1].Question != "b" {
		t.Fatalf("降序 top2 该是 c,b：%+v", top)
	}
	cites := st.TopCitations(5)
	if len(cites) != 0 {
		t.Fatalf("没有 cite 信号时该空：%+v", cites)
	}
	st.Record(Signal{Kind: SignalCitationClick, Target: "d1#rune[0:5]"})
	st.Record(Signal{Kind: SignalCitationClick, Target: "d1#rune[0:5]"})
	if got := st.TopCitations(5); len(got) != 1 || got[0].Note != "2" {
		t.Fatalf("点两次的引用该排第一且计数 2：%+v", got)
	}
}
