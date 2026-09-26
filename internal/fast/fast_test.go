package fast

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

func TestFASTFindsKnownAnswer(t *testing.T) {
	body := strings.Repeat("填充无关内容 padding padding padding。\n", 30) +
		"关键配置：连接池最大 128，超时 30 秒。\n" +
		strings.Repeat("填充无关内容 padding padding padding。\n", 30)
	src := source.New("配置手册", "md", "file://cfg", "cfg", "zh", body, nil)
	other := source.New("无关", "md", "file://x", "x", "zh", "今天天气不错，适合散步。", nil)
	e := New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
	ans, err := e.Search(context.Background(), "连接池最大连接数", []source.Source{src, other})
	if err != nil {
		t.Fatal(err)
	}
	if ans.SourceID != src.ID {
		t.Fatalf("picked %s want %s", ans.SourceID, src.ID)
	}
	if ans.Mode != "FAST" || ans.LLMCalls != 2 {
		t.Fatalf("mode=%s calls=%d", ans.Mode, ans.LLMCalls)
	}
	if ans.Confidence < 0.35 {
		t.Fatalf("confidence %v too low", ans.Confidence)
	}
	if ans.Skipped {
		t.Fatal("good answer must not skip cluster persist")
	}
	if !strings.Contains(ans.Summary, "128") {
		t.Fatalf("summary missing answer: %s", ans.Summary)
	}
}

func TestFASTQualityGateSkipsThinEvidence(t *testing.T) {
	src := source.New("薄", "md", "", "thin", "zh", strings.Repeat("毫无关系的文本。", 200), nil)
	e := New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
	ans, err := e.Search(context.Background(), "连接池最大连接数是多少", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if !ans.Skipped {
		t.Fatalf("thin evidence must skip cluster, conf=%v", ans.Confidence)
	}
}

func TestRankPrefersHigherTf(t *testing.T) {
	a := source.New("a", "md", "", "a", "zh", "缓存 缓存 缓存 其他", nil)
	b := source.New("b", "md", "", "b", "zh", "缓存 其他文本很多", nil)
	ranked := rankSources([]string{"缓存"}, []source.Source{a, b})
	if len(ranked) == 0 || ranked[0].src.ID != a.ID {
		t.Fatalf("want a first, got %+v", ranked)
	}
}

// The deterministic intent gates now back the LLM's chat/doc_summary
// verdicts (see llm.AigateAnalyzer.Analyze): a real question misread as
// chat must fail the gate so the verify pass can correct it, and a genuine
// greeting must pass it so no query pays the extra call.
func TestIntentGates(t *testing.T) {
	chat := []string{"你好", "您好", "在吗", "hello", "hi", "thanks", "谢谢", "再见", "拜拜"}
	for _, q := range chat {
		if !LooksLikeChat(q) {
			t.Errorf("LooksLikeChat(%q)=false, want true", q)
		}
	}
	notChat := []string{"", "什么情况下会被行政拘留", "老师体罚学生有法律支持么", "黑土地保护法对剥离黑土的再利用有些什么要求", "会被行政拘留吗", "你好呀请问治安管理处罚法怎么规定的"}
	for _, q := range notChat {
		if LooksLikeChat(q) {
			t.Errorf("LooksLikeChat(%q)=true, want false", q)
		}
	}
	if !LooksLikeDocSummary("请总结一下这份文档") && !LooksLikeDocSummary("帮我通读全文") {
		t.Error("doc-summary phrasing must pass the gate")
	}
	// "总结一下…"/"翻译全文" match the ORIGINAL offline doc-summary rules
	// (verb + scope) — the extraction must not silently change semantics.
	for _, q := range []string{"总结一下治安管理处罚法的种类", "翻译全文"} {
		if !LooksLikeDocSummary(q) {
			t.Errorf("LooksLikeDocSummary(%q)=false, original rules say true", q)
		}
	}
	for _, q := range []string{"什么情况下会被拘留", "老师体罚学生有法律支持么"} {
		if LooksLikeDocSummary(q) {
			t.Errorf("LooksLikeDocSummary(%q)=true, want false", q)
		}
	}
}

// Stage telemetry: with a Stages hook wired, one search reports all four
// FAST stages; with nil (default, gates) nothing changes. The split exists
// to answer "where did the seconds go" with data.
func TestSearchReportsStages(t *testing.T) {
	body := "关键配置：连接池最大 128，超时 30 秒。\n" + strings.Repeat("填充 padding padding。\n", 40)
	src := source.New("配置手册", "md", "file://cfg", "cfg", "zh", body, nil)
	e := New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
	got := map[string]time.Duration{}
	var mu sync.Mutex
	e.Stages = func(name string, d time.Duration) {
		mu.Lock()
		got[name] = d
		mu.Unlock()
	}
	if _, err := e.Search(context.Background(), "连接池最大连接数", []source.Source{src}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"analyze", "cascade", "sample", "synth"} {
		d, ok := got[want]
		if !ok {
			t.Fatalf("stage %q not reported", want)
		}
		if d < 0 {
			t.Fatalf("stage %q negative duration %v", want, d)
		}
	}
}

// bridgeScorer tells the two sampling passes apart: the plain query's windows
// score 5 (weak but kept), the session-expanded query's windows also score 5 —
// below the raised floor, so the bridge must fail and leave the primary
// windows alone.
type bridgeScorer struct{}

func (bridgeScorer) Score(_ context.Context, query string, _ mcs.Sample) (float64, string, error) {
	if strings.Contains(query, "会话近几问") {
		return 5, "expanded-pass window", nil
	}
	return 5, "primary-pass window", nil
}

// The bridge candidate loop must not reuse the primary kept slice's backing
// array: with weak-but-kept primary windows (4 ≤ score < 7) and a candidate
// that lands below the floor, a failed bridge used to leave the primary
// answer assembled from the candidate's windows — an answer attributed to one
// document, quoting another.
func TestSessionBridgeFailureKeepsPrimaryWindows(t *testing.T) {
	oldFloor := bridgeFloorScore
	bridgeFloorScore = 6.5
	defer func() { bridgeFloorScore = oldFloor }()

	primary := source.New("主文档", "md", "", "primary", "zh",
		strings.Repeat("主文档正文 连接池 128 超时。\n", 60), nil)
	cand := source.New("候选", "md", "", "cand", "zh",
		strings.Repeat("候选文档窗口 饲养动物 吠叫。\n", 60), nil)

	e := New(bridgeScorer{})
	e.SampleContext = "会话近几问"
	e.Usage = map[string]float64{cand.ID: 0.9}
	ans, err := e.Search(context.Background(), "主文档 连接池 128", []source.Source{primary, cand})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Skipped {
		t.Fatalf("kept primary windows must synthesize, conf=%v", ans.Confidence)
	}
	if ans.Bridged {
		t.Fatal("candidate below the floor must not bridge")
	}
	if ans.SourceID != primary.ID {
		t.Fatalf("answer attributed to %s, want %s", ans.SourceID, primary.ID)
	}
	for i, sm := range ans.Samples {
		if strings.Contains(sm.Content, "候选文档窗口") {
			t.Fatalf("sample %d quotes the failed bridge candidate: %q", i, sm.Content)
		}
		if !strings.Contains(sm.Content, "主文档正文") {
			t.Fatalf("sample %d lost the primary window: %q", i, sm.Content)
		}
	}
	if strings.Contains(ans.Summary, "候选") {
		t.Fatalf("summary quotes the failed bridge candidate: %s", ans.Summary)
	}
}

// Cross-script gate fairness (P2 transfer fixes): a Latin greeting word
// must match as a whole word — "hi" inside "which?" is not a greeting, and
// a question eaten by the chat gate never reaches retrieval. The doc-summary
// and question-word gates get the same treatment: English verbs and
// wh-words must route like their Chinese counterparts instead of falling
// through (or misfiring) on script alone.
func TestIntentGatesAreScriptFair(t *testing.T) {
	for _, q := range []string{"which", "this", "thin", "arch?"} {
		if LooksLikeChat(q) {
			t.Errorf("LooksLikeChat(%q)=true — a Latin substring is not a greeting", q)
		}
	}
	for _, q := range []string{"hi", "hello", "thanks"} {
		if !LooksLikeChat(q) {
			t.Errorf("LooksLikeChat(%q)=false, want true", q)
		}
	}
	if !LooksLikeDocSummary("summarize this") {
		t.Error("LooksLikeDocSummary(\"summarize this\")=false — English verb+scope must pass")
	}
	if LooksLikeDocSummary("what is probation") {
		t.Error("LooksLikeDocSummary(\"what is probation\")=true — a question is not a doc-summary op")
	}
	for _, q := range []string{"what is the max connection", "how do I reset"} {
		if !questionShaped(q) {
			t.Errorf("questionShaped(%q)=false — English question routed to filename-only", q)
		}
	}
	for _, q := range []string{"config max connections", "部署手册"} {
		if questionShaped(q) {
			t.Errorf("questionShaped(%q)=true — a noun phrase is not a question", q)
		}
	}
}
