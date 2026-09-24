package ingest

// Text-quality gates shared by every extractor (PDF, and any future candidate).
//
// Why they exist: an extractor's job is to produce text a human would recognise
// as the document. When it cannot, the honest outcome is an empty result that
// the caller records as a skipped file — NOT a half-garbage "source" that every
// later retrieval pays for and whose citations point at noise.
//
// Three independent signals, because each one alone is fooled:
//
//   - textiness: is this text at all? (fooled by a per-glyph dump, which is all
//     letters)
//   - wordiness: is it STRUCTURED text, or one character per line? (fooled by
//     binary that happens to be printable)
//   - topLineShare: is it one line repeated? (fooled by nothing else — this is
//     the watermark/boilerplate case that passes both others)

import (
	"strings"
	"unicode"
)

// minStreamTextiness is the share of a stream that must be text-like before we
// scrape text operators out of it. Measured: real prose 1.00, law/baike/poetry
// corpora 0.99-1.00, embedded font programs and image data 0.30-0.55.
const minStreamTextiness = 0.85

// minDocTextiness is the whole-document bar. Looser than the per-stream bar
// because a real extraction legitimately mixes prose with a little cruft, but a
// half-garbage document must not become a searchable source.
const minDocTextiness = 0.90

// minWordiness is the share of non-space characters that must sit in a run of at
// least two. Measured on real corpora: 0.999-1.000. A CID/Type0-font PDF
// shatters one glyph per text-showing operator, so its extraction is one
// character per line: wordiness 0.42. textiness alone cannot see this — the
// characters are all letters — so both signals are required.
const minWordiness = 0.90

// maxTopLineShare is how much of a document one repeated line may occupy before
// we treat the extraction as boilerplate rather than content.
const maxTopLineShare = 0.5

// textiness is the share of runes that are letters, digits, CJK, common
// punctuation or whitespace — i.e. the share a human would read as text.
func textiness(s string) float64 {
	if s == "" {
		return 0
	}
	good := 0
	for _, r := range s {
		switch {
		case r == ' ' || r == '\n' || r == '\r' || r == '\t':
			good++
		case r >= 0x4e00 && r <= 0x9fff: // CJK unified ideographs
			good++
		case r >= 0x3040 && r <= 0x30ff: // kana
			good++
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			good++
		case unicode.IsPunct(r):
			good++
		}
	}
	return float64(good) / float64(len([]rune(s)))
}

// wordiness is the share of non-space characters that belong to a run of two or
// more non-space characters. Real prose (including CJK without spaces) is ~1.0;
// a per-glyph dump collapses toward 0.
func wordiness(s string) float64 {
	nonSpace := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			nonSpace++
		}
	}
	if nonSpace == 0 {
		return 0
	}
	inRuns, run := 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			run = 0
			continue
		}
		run++
		if run == 2 {
			inRuns += 2 // this char and the one that opened the run
		} else if run > 2 {
			inRuns++
		}
	}
	return float64(inRuns) / float64(nonSpace)
}

// topLineShare is the share of non-empty lines equal to the most frequent one.
func topLineShare(s string) float64 {
	counts := map[string]int{}
	total, top := 0, 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		counts[line]++
		if counts[line] > top {
			top = counts[line]
		}
		total++
	}
	if total < 32 || top <= 1 {
		// Too small to judge, or simply no repetition.
		return 0
	}
	return float64(top) / float64(total)
}

// gateText applies all three document-level gates. It is exported for the
// extractors that live outside this package's internals.
func gateText(s string) string {
	res := strings.TrimSpace(s)
	if res == "" {
		return ""
	}
	if textiness(res) < minDocTextiness || wordiness(res) < minWordiness {
		return ""
	}
	if topLineShare(res) > maxTopLineShare {
		return ""
	}
	return res
}
