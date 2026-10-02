// Package charset turns raw document bytes into the UTF-8 text the rest of the
// suite stores, and refuses — loudly — when it cannot.
//
// WHY THIS EXISTS (measured on a 1,251-file Chinese web-novel corpus):
//
//	GB18030          1,088 files  87.0%   → transcode once, at ingest
//	pure UTF-8          68 files   5.4%   → keep as-is
//	undecodable         95 files   7.6%   → REFUSE (previously stored as mojibake)
//	silent mis-decode    ~5 files  0.4%   → detectable in principle, NOT handled here
//
// RE-COMPUTED 2026-10-03 at HEAD 9a9d0a6 by `TestCorpusTierCensus`
// (CHARSET_CORPUS=<corpus> go test ./internal/charset/ -run TestCorpusTierCensus),
// which runs the shipped Decode over the tree rather than restating a table.
// It reproduced docs/perf-plan.md §5.3's table exactly and contradicted the
// 1,150/33 split this comment previously carried — that older split is what
// drifted, not the code. The same section's heading ("98.2% 非 UTF-8") matches
// neither: 1,088 + 95 = 1,183 of 1,251 = 94.6%.
//
// Census caveat, because it is the kind that silently eats 2.5% of a sample:
// the corpus is 1,220 `.txt` + 31 `.TXT`. A case-sensitive `.txt` filter drops
// the 31 uppercase ones, and those are the Windows-era downloads — precisely
// the likely-GB18030 tail.
//
// The ~5 silent mis-decodes sit INSIDE the 1,088 accepted tier (they decode
// cleanly), which is why the three measurable tiers sum to exactly 1,251.
//
// Without this, a GBK novel is stored byte-for-byte and read as UTF-8: every
// CJK character becomes one RuneError, the document's rune count is wrong
// (which silently corrupts block boundaries and citation offsets), and a
// Chinese query can never match it. The failure is invisible — nothing in the
// pipeline reports it.
//
// The reference implementation (sirchmunk) has a charset-detection layer
// (charset_normalizer) that stores its result in FileInfo.encoding, and then
// reads every document with a hardcoded `encoding="utf-8", errors="replace"`.
// The detected value is never used, and `errors="replace"` converts 73.8% of a
// real GBK novel into U+FFFD — destroying the bytes at read time, so a later
// transcode cannot recover them. That is the pattern this package exists to
// avoid: NEVER substitute a character for bytes you failed to decode.
//
// TIER 3 (silent mis-decode) is deliberately NOT handled. Those five files
// decode as GB18030 without error but contain regions written in another
// encoding, so the output is wrong yet clean. Distinguishing them needs a
// signal validated on far more than five samples, and a rule fitted to five
// points would be exactly the kind of guess this repo does not ship. When the
// sample grows, add a detector here and gate it — do not widen the tier
// silently.
package charset

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// Tier is the outcome of decoding, and is recorded on the document so an
// operator can tell a converted corpus from a native one.
type Tier string

const (
	// TierUTF8: the bytes were already valid UTF-8 and are kept unchanged.
	TierUTF8 Tier = "utf-8"
	// TierGB18030: the bytes were decoded to UTF-8. GB18030 is a superset of
	// GBK and GB2312, so one decoder covers the whole family; recording the
	// family rather than the guess keeps the metadata honest.
	TierGB18030 Tier = "gb18030"
	// TierUndecodable: neither tier applied. Such a document must NOT be
	// stored — see the package doc for what happens if it is.
	TierUndecodable Tier = "undecodable"
)

// ErrUndecodable is returned for bytes that are neither valid UTF-8 nor
// decodable as GB18030. It is an ERROR on purpose: the alternative — a
// replacement character — produces a document that looks stored and answers
// queries while being unreadable.
var ErrUndecodable = errors.New("charset: bytes are neither valid UTF-8 nor GB18030")

// Result is a decoded document plus the provenance needed to audit the
// conversion later.
type Result struct {
	Text string
	Tier Tier
	// SrcBytes is the length of the input, before conversion.
	SrcBytes int
	// SrcDigest is the sha256 of the INPUT bytes, so the store can prove
	// later which file on disk produced this text. The suite's document
	// Digest covers the CONVERTED + normalized body, so without this the
	// file-to-engine link is unverifiable.
	SrcDigest string
	// Converted is true when the bytes changed. A transcoded corpus and a
	// native one must be distinguishable in a report without diffing text.
	Converted bool
}

// Decode converts raw document bytes to UTF-8, or refuses.
//
// It never substitutes: there is no `errors="replace"` path here and there
// must not be one added. A caller that wants a best-effort preview (a scan
// headline, say) should ask for a bounded prefix of the input and decode that,
// rather than being handed U+FFFD for the whole document.
func Decode(raw []byte) (Result, error) {
	res := Result{SrcBytes: len(raw), SrcDigest: digestHex(raw)}

	// Strip a UTF-8 BOM rather than storing it: a BOM is an encoding marker,
	// not content, and leaving it in makes the first token of every query
	// mismatch. Recorded as tier utf-8 either way.
	body := bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(body) {
		res.Text = string(body)
		res.Tier = TierUTF8
		return res, nil
	}

	// THE IMPORTANT PART.
	//
	// x/text's GB18030 decoder does NOT report failure — not through
	// transform.Bytes, not through the Transformer with atEOF=true, and not
	// through atEOF=false either (where the error is an informational "more
	// input expected", not a failure). Verified on the three shapes:
	//
	//	{0xB1}          -> "�"     err=nil   (truncated sequence)
	//	{0xFF,0xFE,...} -> "����"   err=nil   (unmappable)
	//	{0x81,0x20}     -> "� "    err=nil   (illegal trail byte)
	//
	// So there is no API path that tells us the decode was lossy, and the naive
	// transform.Bytes call IS an errors="replace" implementation wearing a
	// library-shaped interface. Wiring it in unmodified would reproduce the
	// reference implementation's failure exactly: the bytes are destroyed at
	// read time, no call site reports it, and U+FFFD flows on into windows,
	// citations and scores where nothing looks wrong.
	//
	// Two checks, in cost order:
	//
	//  1. Fast path — the decoder's only substitution character is U+FFFD, so
	//     a U+FFFD-free output is provably lossless. One linear scan, no second
	//     pass, and it localises the failure for the error message.
	//  2. Authoritative — re-encode the decoded text and compare with the
	//     input. This is the real invariant ("the conversion was lossless")
	//     rather than a proxy for it, and it is what catches a substitution
	//     that is not U+FFFD. Run only when the fast path already found
	//     damage, so the cost is paid only on the failure path.
	out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), body)
	if err != nil {
		res.Tier = TierUndecodable
		return res, fmt.Errorf("%w: %v", ErrUndecodable, err)
	}
	if i := strings.IndexRune(string(out), utf8.RuneError); i >= 0 {
		res.Tier = TierUndecodable
		// i is a BYTE offset (strings.IndexRune) — reporting it next to a rune
		// count produced "rune 1284522 of 921356", an index larger than the
		// total, which sends an operator hunting for a position that cannot
		// exist. Report both in the same unit.
		detail := fmt.Sprintf("decoder substituted U+FFFD at byte %d of %d", i, len(out))
		if back, _, rerr := transform.Bytes(simplifiedchinese.GB18030.NewEncoder(), out); rerr != nil || !bytes.Equal(back, body) {
			detail += "; re-encode round trip also differs (lossy conversion confirmed)"
		}
		return res, fmt.Errorf("%w: %s", ErrUndecodable, detail)
	}
	res.Text = string(out)
	res.Tier = TierGB18030
	res.Converted = !bytes.Equal(raw, out)
	return res, nil
}

// Preview decodes at most limit bytes of raw for display purposes.
//
// Two distinct situations are handled separately, and conflating them is how a
// preview ends up lying:
//
//   - The caller CUT the input mid-character. That is an artefact of the cut,
//     not of the document, so the partial tail is dropped and the clean prefix
//     is returned. Up to maxTruncationTail bytes are retried, which covers
//     every multi-byte length GB18030 uses.
//   - The bytes are genuinely undecodable. Then there is no honest preview and
//     Preview returns ("", TierUndecodable) so the CALLER decides what to
//     display. It never hands back damaged bytes dressed as content — the
//     reference implementation's `errors="replace"` is precisely that, and a
//     preview that shows U+FFFD is indistinguishable from a preview of a
//     genuinely damaged file.
//
// This is not a path to storing a document. Decode is, and Decode refuses
// rather than substituting.
func Preview(raw []byte, limit int) (string, Tier) {
	if limit > 0 && len(raw) > limit {
		raw = raw[:limit]
	}
	if res, err := Decode(raw); err == nil {
		// A prefix that decoded but ended in U+FFFD cannot happen — Decode
		// refuses that — so this is already clean.
		return res.Text, res.Tier
	}
	// Retry without up to 3 trailing bytes: that distinguishes a cut through a
	// multi-byte character from real damage.
	for drop := 1; drop <= maxTruncationTail && drop < len(raw); drop++ {
		if res, err := Decode(raw[:len(raw)-drop]); err == nil {
			return res.Text, res.Tier
		}
	}
	return "", TierUndecodable
}

// maxTruncationTail is GB18030's longest encoding form (4 bytes), so a preview
// never retries more tail than a single character could occupy.
const maxTruncationTail = 3
