package fast

import (
	"strings"
	"unicode"

	"github.com/willove/cumulus/internal/mcs"
)

// scaffolding are the characters a deterministic template contributes around
// the content: the 【摘要】/【来源】 headers, the [1] (…) citation lines.
const scaffolding = "【】（）()[]来源摘 要编号…·"

// minAnswerRunes is how much content an answer must contribute beyond the
// words the question already used. Two is deliberately low: a real answer can
// legitimately be short ("128", "是西海龙王三太子"), and the job here is to
// catch an answer that is nothing but the question again — not to judge quality.
const minAnswerRunes = 2

// TemplateDegraded reports whether s is the deterministic scaffold rather than
// a synthesizer answer. Exported because the persistence boundary must be able
// to check it directly: it cannot rely on ans.Refused having survived every hop
// between the synthesizer and the store (a real run carried Refused=false with
// a template summary, and the refusal template was persisted as a cluster's
// content).
func TemplateDegraded(s string) bool {
	return strings.HasPrefix(s, "【") && strings.Contains(s, "摘要】")
}

// Answerable reports whether a summary carries any answer content at all.
//
// It is a stopgap with a narrow, evidence-backed job: reject the one shape
// observed end-to-end on a real 727k-rune novel (5/5 questions wrong, latency
// fine) where the deterministic template was persisted as a cluster's content,
// whose LevelKeys[principle] became the question itself — so the next
// same-topic query replayed "孙悟空的兵器是什么" as knowledge, for 0 tokens.
//
//	"【摘要】<question>\n【来源】<title>\n[1] (…) <quote>"   → false
//
// A synthesiser never emits the 【摘要】 header, so this cannot misfire on a real
// answer — including a one-word one.
//
// What it does NOT do is judge whether the content is correct. That is the
// deeper problem and it lives elsewhere: the existing gates — RelevanceGate on
// the evidence, and the confidence floor — both measure whether the passage
// CONTAINS the question's words, and a passage can match a question
// lexically, score well on both, and still not answer it. Closing that needs a
// real answerability criterion, not a shape check.
func Answerable(query, summary string) bool {
	return !TemplateDegraded(summary)
}

// EchoesQuery reports whether a summary is the question restated with no
// content of its own.
//
// NOT wired into the persistence gate, deliberately. It is a plausible shape —
// but its only evidence would be a test this package wrote itself, unlike
// TemplateDegraded which came out of a real run. It also has a demonstrated
// false positive: the existing fixture with question "alpha and beta" and
// answer "alpha beta" is a legitimate short answer that this predicate calls an
// echo. Shipping a check justified only by a self-authored test, at the cost of
// a known false positive, is the trade this repo does not make. It stays here
// as a predicate for whoever designs the real answerability criterion.
func EchoesQuery(query, summary string) bool {
	fromQuery := map[rune]bool{}
	for _, f := range mcs.Fields(query) {
		for _, r := range f {
			fromQuery[r] = true
		}
	}
	content := 0
	for _, r := range summary {
		if unicode.IsSpace(r) || strings.ContainsRune(scaffolding, r) {
			continue
		}
		if fromQuery[r] {
			continue
		}
		content++
	}
	return content < minAnswerRunes
}
