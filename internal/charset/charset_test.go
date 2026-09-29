package charset

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// gb18030 encodes a string to the legacy family so the tests exercise the real
// conversion rather than hand-made invalid bytes.
func gb(t *testing.T, s string) []byte {
	t.Helper()
	out, err := gbEncode(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDecodeUTF8PassesThroughUnchanged(t *testing.T) {
	body := "连接池最大连接数是多少？"
	res, err := Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != body {
		t.Fatalf("valid UTF-8 must be untouched: %q", res.Text)
	}
	if res.Tier != TierUTF8 {
		t.Fatalf("tier = %q, want utf-8", res.Tier)
	}
	if res.Converted {
		t.Fatal("a UTF-8 input must not be reported as converted")
	}
	// Provenance must be present even on the no-op tier, or a store cannot
	// prove which file produced the text.
	if res.SrcDigest == "" || res.SrcBytes != len(body) {
		t.Fatalf("provenance missing on the utf-8 tier: %+v", res)
	}
}

func TestDecodeStripsBOMAndSaysUTF8(t *testing.T) {
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte("第一章")...)
	res, err := Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != TierUTF8 {
		t.Fatalf("a BOM-prefixed UTF-8 file is still UTF-8, got %q", res.Tier)
	}
	if strings.HasPrefix(res.Text, "\ufeff") {
		t.Fatalf("BOM must not survive into the stored text: %q", res.Text)
	}
	if res.SrcBytes != len(body) {
		t.Fatalf("SrcBytes must count the input including the BOM: %d", res.SrcBytes)
	}
}

func TestDecodeGB18030Converts(t *testing.T) {
	want := "御用兵王 第1章 正文"
	res, err := Decode(gb(t, want))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != want {
		t.Fatalf("round trip changed the text:\n got %q\nwant %q", res.Text, want)
	}
	if res.Tier != TierGB18030 {
		t.Fatalf("tier = %q, want gb18030", res.Tier)
	}
	if !res.Converted {
		t.Fatal("a converted document must be reported as converted")
	}
	if !utf8.ValidString(res.Text) {
		t.Fatal("output must be valid UTF-8")
	}
}

func TestDecodeNeverSubstitutesCharacters(t *testing.T) {
	// The one behaviour that must never be added: on failure, return a
	// replacement character and let it flow downstream. Assert the opposite —
	// an undecodable input yields an ERROR and no text.
	// These are the exact inputs that made x/text's decoder emit U+FFFD with
	// err == nil, i.e. the real hazard rather than an invented one.
	garbage := []byte{0xFF, 0xFE, 0xFF, 0xFF, 0x00, 0x80, 0x81, 0x8D}
	res, err := Decode(garbage)
	if err == nil {
		t.Fatalf("undecodable bytes must return an error, got tier %q text %q", res.Tier, res.Text)
	}
	if !strings.Contains(err.Error(), "neither valid UTF-8 nor GB18030") {
		t.Fatalf("the error must name both tiers it tried: %v", err)
	}
	if res.Tier != TierUndecodable {
		t.Fatalf("tier = %q, want undecodable", res.Tier)
	}
	if strings.ContainsRune(res.Text, utf8.RuneError) {
		t.Fatal("no U+FFFD may ever appear in Decode output")
	}
	// The caller must not be able to accidentally store the empty result as if
	// it were a document.
	if res.Text != "" {
		t.Fatalf("an undecodable input must yield no text to store, got %q", res.Text)
	}
}

// A document that is 99% GBK with a few stray bytes still decodes as GB18030 in
// most byte sequences. What must NOT happen is the store believing such a file
// is clean UTF-8 — the tier has to reflect the actual conversion.
func TestDecodeTierReflectsActualConversion(t *testing.T) {
	mixed := append(gb(t, "这是中文"), []byte{0xFF, 0xFE}...)
	res, err := Decode(mixed)
	if err != nil {
		// Acceptable outcome, and the tier must say so.
		if res.Tier != TierUndecodable {
			t.Fatalf("failed decode reported tier %q", res.Tier)
		}
		return
	}
	if res.Tier == TierUTF8 {
		t.Fatal("bytes that are not valid UTF-8 must never be tiered utf-8")
	}
	if !res.Converted {
		t.Fatal("a GB18030 tier must always report converted")
	}
}

func TestDecodeRuneCountIsMeaningful(t *testing.T) {
	// The reason this package exists downstream: rune counts, block offsets
	// and citation ranges are all computed on the decoded text. Decoding GBK
	// as UTF-8 gives one RuneError per byte, so a count taken before decoding
	// is not merely ugly, it is wrong by a large factor.
	src := "连接池最大连接数" + strings.Repeat("配置说明", 200)
	raw := gb(t, src)

	naive := utf8.RuneCount(raw) // what happens without this package
	res, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	real := utf8.RuneCountInString(res.Text)
	if real != len([]rune(src)) {
		t.Fatalf("decoded rune count %d, want %d", real, len([]rune(src)))
	}
	if naive <= real {
		t.Fatalf("the naive count (%d) should be far larger than the real one (%d)", naive, real)
	}
	if naive < real+real/2 {
		t.Fatalf("expected a large divergence to document the hazard, got naive=%d real=%d", naive, real)
	}
}

func TestDecodeEmpty(t *testing.T) {
	res, err := Decode(nil)
	if err != nil {
		t.Fatalf("empty input is valid UTF-8, not an error: %v", err)
	}
	if res.Text != "" || res.Tier != TierUTF8 {
		t.Fatalf("empty input = %+v", res)
	}
}

func TestPreviewTrimsOnlyTheTruncatedTail(t *testing.T) {
	full := "御用兵王第一章"
	raw := gb(t, full)

	// Cut mid-character: the preview must not show U+FFFD for the partial
	// tail, and must not lose the complete characters before it.
	for cut := 1; cut < len(raw); cut++ {
		got, tier := Preview(raw, cut)
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Fatalf("cut=%d produced U+FFFD in a preview: %q", cut, got)
		}
		if tier == TierUndecodable {
			// A 1-3 byte cut can leave nothing decodable; that is the honest
			// answer and the caller decides what to show.
			if got != "" {
				t.Fatalf("cut=%d: undecodable preview must be empty, got %q", cut, got)
			}
			continue
		}
		if !strings.HasPrefix(full, got) {
			t.Fatalf("cut=%d preview is not a prefix of the document: %q", cut, got)
		}
	}
	// The full read must be exact.
	if got, _ := Preview(raw, 0); got != full {
		t.Fatalf("unbounded preview = %q, want %q", got, full)
	}
}

func TestPreviewOfUndecodableIsBoundedNotWhole(t *testing.T) {
	garbage := bytes.Repeat([]byte{0xFF, 0xFE, 0x80}, 1000)
	got, tier := Preview(garbage, 30)
	if tier != TierUndecodable {
		t.Fatalf("tier = %q, want undecodable", tier)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("a preview must never display U+FFFD, got %q", got)
	}
	if len(got) > 30 {
		t.Fatalf("preview exceeded its bound: %d > 30", len(got))
	}
}

func TestDecodeProvenanceDigestMatchesInput(t *testing.T) {
	raw := gb(t, "溯源校验")
	res, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.SrcDigest != digestHex(raw) {
		t.Fatal("SrcDigest must be the digest of the INPUT bytes, not the output")
	}
	if res.SrcBytes != len(raw) {
		t.Fatalf("SrcBytes = %d, want %d", res.SrcBytes, len(raw))
	}
	// Two different source files with identical converted text must have
	// different SrcDigests — that is what keeps them distinguishable.
	other, err := Decode(gb(t, "溯源校验 "))
	if err != nil {
		t.Fatal(err)
	}
	if other.SrcDigest == res.SrcDigest {
		t.Fatal("SrcDigest collides for different inputs")
	}
}

// TestDecodeLosslessRoundTrip is the authoritative form of the tier-2 gate.
// The fast path (no U+FFFD) is a proxy for losslessness; this asserts the real
// invariant — a document that claims to be GB18030 must re-encode to exactly
// the bytes it came from. It is the check that would survive a future decoder
// choosing a different substitution character.
func TestDecodeLosslessRoundTrip(t *testing.T) {
	// Every GB18030 document that survives the gate must round-trip.
	for _, want := range []string{
		"御用兵王 第一卷 记忆的倒影",
		"连接池最大连接数是多少？",
		"§1 mixed ASCII 与全角标点，以及换行\n第二行\t制表符",
	} {
		raw := gb(t, want)
		res, err := Decode(raw)
		if err != nil {
			t.Fatalf("%q: %v", want, err)
		}
		if res.Text != want {
			t.Fatalf("decode changed the text:\n got %q\nwant %q", res.Text, want)
		}
		back := gb(t, res.Text)
		if string(back) != string(raw) {
			t.Fatalf("round trip differs for %q:\n got %x\nwant %x", want, back, raw)
		}
	}
}

// A rejected document must say so in a way that distinguishes "the decoder
// substituted" from "the input was not GB18030 at all" — the error is read by
// an operator deciding whether to fix the file or the pipeline.
func TestDecodeErrorDistinguishesLossyFromAbsent(t *testing.T) {
	_, err := Decode([]byte{0xFF, 0xFE, 0xFF, 0xFF})
	if err == nil {
		t.Fatal("expected refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "substituted U+FFFD") {
		t.Fatalf("the error must name the observed damage: %v", err)
	}
	if !strings.Contains(msg, "round trip") {
		t.Fatalf("the error must record that losslessness was confirmed by round trip: %v", err)
	}
}
