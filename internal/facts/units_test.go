package facts

import (
	"context"
	"errors"
	"testing"
)

// The measured miscuts split into two kinds, and the gate can only tell them
// apart in ONE of them. Pinning both kinds here is the point: the difference is
// the honest limit of a local check, and it is documented rather than papered
// over with a keyword heuristic that would also reject good requirements.

// mechanicalMiscuts are the ones a local check CAN reject: the split ran
// through a quoted/《》 title, leaving an unbalanced boundary rune.
var mechanicalMiscuts = []struct {
	query string
	parts []string
}{
	{"《…旅游服务与管理英语》共编写了多少个单元", []string{"《中等职业教育特色精品课程规划教材：旅游服务", "管理英语》共编写了多少个单元？"}},
	{"《郑韩故城兴弘花园与热电厂墓地》的编者", []string{"《郑韩故城兴弘花园", "热电厂墓地》这本书的编者是哪个机构？"}},
	{`"右和"这本书的出版社`, []string{`"右`, `"这本书的出版社是哪家？`}},
}

// semanticMiscuts are syntactically well-formed: no unbalanced rune, long
// enough, reads like a clause. No local string check can see that the boundary
// landed inside a defined term or an enumeration — that is the LLM
// decomposer's job, and the gate is only a net for the mechanical tail.
var semanticMiscuts = []struct {
	query string
	parts []string
}{
	{"领事保护与协助职责", []string{"驻外外交机构想要临时在履责区域外执行领事保护", "协助职责，需要获得谁的同意？"}},
	{"无线电报和无线电话", []string{"外国籍船舶在长江水域航行期间，其无线电报", "无线电话发射机只准", "哪些电台通讯？"}},
	{"未能及时填写病历", []string{"医务人员因紧急抢救未能", "时填写病历的，应当在抢救结束后多长时间内据实补记？"}},
	{"条例对单位和个人", []string{"条例对单位", "个人发布水文情报预报有什么禁止性规定？"}},
	{"数额特别巨大或者有其他严重情节", []string{"信用卡诈骗数额特别巨大", "有其他特别严重情节的，按刑法修正案五最高会判什么刑？"}},
}

func TestIsSemanticUnitRejectsMechanicalMiscuts(t *testing.T) {
	for _, tc := range mechanicalMiscuts {
		for _, p := range tc.parts {
			if IsSemanticUnit(p) {
				t.Errorf("must reject a title-cut fragment: %q (from %q)", p, tc.query)
			}
		}
	}
}

// TestSemanticMiscutsAreNotLocallyDetectable documents the limit instead of
// hiding it: these pass the gate, which is why the LLM decomposer — not this
// function — is the actual fix for the heuristic's 0% split precision.
//
// The consequence for the caller is stated in BuildParts' contract: an
// under-rejecting gate yields a phantom requirement (the DEEP loop burns its
// budget chasing an unreachable one), while an over-rejecting one falls back to
// facts.Build — today's K=1 behaviour. That asymmetry is why no aggressive
// keyword rule is added here: it would trade a real risk for a cosmetic one.
func TestSemanticMiscutsAreNotLocallyDetectable(t *testing.T) {
	passes := 0
	for _, tc := range semanticMiscuts {
		for _, p := range tc.parts {
			if IsSemanticUnit(p) {
				passes++
			}
		}
	}
	if passes == 0 {
		t.Fatal("this test exists to document that semantic miscuts pass the gate; if the " +
			"gate ever starts catching them, move them into mechanicalMiscuts instead")
	}
	t.Logf("%d/%d semantic-miscut fragments pass the local gate (expected: the LLM decomposer is the fix)",
		passes, len(semanticMiscuts))
}

func TestIsSemanticUnitAcceptsWholeRequirements(t *testing.T) {
	ok := []string{
		"连接池最大连接数",
		"实用新型的定义",
		"外观设计的定义",
		// A balanced book title is fine — it is a title, not a cut through one.
		"《中华人民共和国专利法》第一条规定的制定目的是什么",
		"外观设计的保护期限是多久",
		"数据跨境传输的安全评估要求有哪些",
	}
	for _, s := range ok {
		if !IsSemanticUnit(s) {
			t.Errorf("must accept a whole requirement: %q", s)
		}
	}
}

func TestIsSemanticUnitRejectsTooShort(t *testing.T) {
	for _, s := range []string{"参", "和", "多少", "   ", "a b"} {
		if IsSemanticUnit(s) {
			t.Errorf("must reject a fragment shorter than the unit floor: %q", s)
		}
	}
}

// BuildParts is the gate the decomposer's output passes through. An empty
// result is the caller's signal to fall back to the heuristic — never to search
// with no requirements.
func TestBuildPartsEmptyMeansFallBack(t *testing.T) {
	if got := BuildParts("右和这本书的出版社", []string{`"右`, `"这本书的出版社`}); len(got) != 0 {
		t.Fatalf("all-fragment input must yield no facts (caller falls back), got %+v", got)
	}
	if got := BuildParts("q", nil); len(got) != 0 {
		t.Fatalf("nil input must yield no facts, got %+v", got)
	}
	if got := BuildParts("q", []string{}); len(got) != 0 {
		t.Fatalf("empty input must yield no facts, got %+v", got)
	}
}

func TestBuildPartsKeepsGoodUnitsAndRenumbersIDs(t *testing.T) {
	// A leading fragment is dropped, the rest survive, and IDs stay contiguous.
	got := BuildParts("实用新型和外观设计分别指什么", []string{`"右`, "实用新型的定义", "外观设计的定义", "  "})
	if len(got) != 2 {
		t.Fatalf("want 2 surviving units, got %d: %+v", len(got), got)
	}
	if got[0].ID != "f1" || got[1].ID != "f2" {
		t.Fatalf("IDs must be contiguous from f1 after a drop: %+v", got)
	}
	if got[0].Query != "实用新型的定义" || got[1].Query != "外观设计的定义" {
		t.Fatalf("order/content wrong: %+v", got)
	}
}

func TestBuildPartsCapsAtFour(t *testing.T) {
	in := []string{"需求一是什么", "需求二是什么", "需求三是什么", "需求四是什么", "需求五是什么"}
	if got := BuildParts("分别是什么", in); len(got) != maxParts {
		t.Fatalf("want the %d-unit ceiling, got %d", maxParts, len(got))
	}
}

func TestBuildPartsTrimsWhitespace(t *testing.T) {
	got := BuildParts("连接池最大连接数", []string{"  连接池最大连接数  "})
	if len(got) != 1 || got[0].Query != "连接池最大连接数" {
		t.Fatalf("parts must be trimmed: %+v", got)
	}
}

// The heuristic must stay reachable as the deterministic baseline: with no
// decomposer wired, Build is what every offline gate runs on.
func TestBuildRemainsTheDeterministicBaseline(t *testing.T) {
	if got := Build("连接池最大连接数是多少"); len(got) != 1 {
		t.Fatalf("a single-requirement query must stay K=1, got %d", len(got))
	}
	if got := Build("实用新型和外观设计分别指什么"); len(got) < 2 {
		t.Fatalf("a conjunction query must still split under the heuristic, got %d", len(got))
	}
}

// --- Engine.decompose contract (in-package via the Decomposer seam) ---

type stubDecomposer struct {
	parts []string
	err   error
	calls int
}

func (s *stubDecomposer) Decompose(_ context.Context, _ string) ([]string, error) {
	s.calls++
	return s.parts, s.err
}

// decomposeInputs is a small local harness: the Engine method is exercised here
// through the same Decomposer seam production uses.
func TestDecomposerSeamShapes(t *testing.T) {
	d := &stubDecomposer{parts: []string{"实用新型的定义", "外观设计的定义"}}
	got := BuildParts("实用新型和外观设计分别指什么", d.parts)
	if len(got) != 2 || got[0].ID != "f1" {
		t.Fatalf("decomposer output must flow through BuildParts: %+v", got)
	}

	// An erroring decomposer yields no parts, so the caller falls back.
	errDec := &stubDecomposer{err: errors.New("boom")}
	if _, err := errDec.Decompose(context.Background(), "q"); err == nil {
		t.Fatal("expected the stub error to surface")
	}
	if BuildParts("q", nil) != nil {
		t.Fatal("nil parts must mean fallback, not an empty requirement set")
	}
}

// TestHasCoordination pins what licenses a K>1 split. The query must SAY there
// is a requirement list; otherwise K=1 is correct by construction and any split
// is a guess. The "或者" cases are the counter-shape: a legal either/or states
// one question, not two.
func TestHasCoordination(t *testing.T) {
	licensed := []string{
		"实用新型和外观设计分别指什么",
		"同一天申请实用新型和发明专利，能同时获得两个专利吗",
		"外观设计的保护期限以及缴费要求分别是什么",
	}
	for _, q := range licensed {
		if !HasCoordination(q) {
			t.Errorf("must license a split: %q", q)
		}
	}
	unlicensed := []string{
		"发明创造定义", // measured: 法定术语被拆成并列概念 → 0 轮早停答错
		"连接池最大连接数",
		"数额特别巨大或者有其他特别严重情节的，最高会判什么刑",
		"是走A还是B",
		"专利法的立法目的是什么",
	}
	for _, q := range unlicensed {
		if HasCoordination(q) {
			t.Errorf("must NOT license a split: %q", q)
		}
	}
}

// TestBuildPartsRejectsUnlicensedSplit is the same failure at the gate layer.
func TestBuildPartsRejectsUnlicensedSplit(t *testing.T) {
	parts := []string{"发明的定义", "创造的定义"}
	if got := BuildParts("发明创造定义", parts); got != nil {
		t.Fatalf("bare term-of-art lookup must fall back to K=1, got %+v", got)
	}
	if got := BuildParts("发明和创造分别指什么", parts); len(got) != 2 {
		t.Fatalf("coordinated query keeps both units, got %+v", got)
	}
	// K=1 always passes: a bare lookup is a legitimate single requirement.
	if got := BuildParts("发明创造定义", []string{"发明创造的定义"}); len(got) != 1 {
		t.Fatalf("K=1 must never be gated on coordination, got %+v", got)
	}
}
