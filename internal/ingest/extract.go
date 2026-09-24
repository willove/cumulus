package ingest

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
)

// ExtractHTML turns an HTML document into plain text for body ingestion
// (Path A). v1 scope, stdlib-only: script/style/comment stripping, tag
// removal, block-tag paragraph breaks, basic entity decoding. DOCX is tree-in
// (ExtractDOCX below); PDF is best-effort (ExtractPDF — encryption, CID/CJK
// fonts and xref-stream PDFs stay on the external-worker shape).
var (
	scriptRe  = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)[^>]*>`)
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	blockRe   = regexp.MustCompile(`(?i)</(p|div|li|tr|h[1-6]|section|article|blockquote|table)>`)
	brRe      = regexp.MustCompile(`(?i)<br\s*/?>`)
	tagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
	blankRe   = regexp.MustCompile(`\n{3,}`)

	entities = map[string]string{
		"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`,
		"&#39;": "'", "&#x27;": "'", "&apos;": "'", "&nbsp;": " ",
	}
)

func ExtractHTML(raw string) string {
	s := scriptRe.ReplaceAllString(raw, " ")
	s = commentRe.ReplaceAllString(s, " ")
	s = brRe.ReplaceAllString(s, "\n")
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	for k, v := range entities {
		s = strings.ReplaceAll(s, k, v)
	}
	return strings.TrimSpace(blankRe.ReplaceAllString(s, "\n\n"))
}

// ExtractDOCX reads a .docx (OOXML zip) and returns paragraph text. Stdlib
// only (archive/zip + encoding/xml); tables/textboxes come along as
// <w:p> paragraphs. Corruption surfaces as an error — no silent empties.
func ExtractDOCX(raw []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("docx: not a zip container: %w", err)
	}
	var paras []string
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("docx: open document.xml: %w", err)
		}
		paras = append(paras, docxParagraphs(rc)...)
		rc.Close()
	}
	if len(paras) == 0 {
		return "", fmt.Errorf("docx: no text in word/document.xml")
	}
	return strings.Join(paras, "\n"), nil
}

func docxParagraphs(r io.Reader) []string {
	dec := xml.NewDecoder(r)
	var paras []string
	var inP bool
	var buf strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "p" {
				inP = true
				buf.Reset()
			}
		case xml.CharData:
			if inP {
				buf.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == "p" {
				inP = false
				if s := strings.TrimSpace(buf.String()); s != "" {
					paras = append(paras, s)
				}
			}
		}
	}
	return paras
}

var (
	pdfStreamRe = regexp.MustCompile(`(?s)stream\r?\n(.*?)endstream`)
	pdfTextRe   = regexp.MustCompile(`\((?:[^()\\]|\\.)*\)`)
)

// ExtractPDF is a best-effort pure-Go text pull for simple PDFs: inflates
// FlateDecode streams and lifts text-showing operators (Tj/TJ/'").
// Encryption, CID/CJK fonts, and xref-stream PDFs are out of scope — those
// go to the external-worker shape. Only text ever lands in the engine.
//
// Streams are filtered by "textiness" BEFORE the text operators are scraped.
// Without that filter every Flate stream was inflated — embedded font programs
// and images included — and the (...) regex then scraped "text" out of binary
// blobs. Two real-world PDFs produced 525K and 2.4M characters of noise
// (textiness 0.51 / 0.43 vs 1.00 for real prose) that was silently stored as a
// searchable source. A stream that is not mostly text is now skipped, and a
// result that is still mostly noise yields "" so the caller's skip accounting
// records it instead of poisoning the corpus.
func ExtractPDF(raw []byte) string {
	var out strings.Builder
	for _, m := range pdfStreamRe.FindAllStringSubmatchIndex(string(raw), -1) {
		data := string(raw)[m[2]:m[3]]
		plain := []byte(data)
		if zr, err := zlib.NewReader(strings.NewReader(data)); err == nil {
			if b, rerr := io.ReadAll(io.LimitReader(zr, 32<<20)); rerr == nil {
				plain = b
			}
			zr.Close()
		}
		inflated := string(plain)
		// Only text-carrying streams. Binary streams (fonts, images, ICC
		// profiles) inflate fine and are the main source of garbage; a stream
		// that survives textiness but shatters one glyph per line is a
		// CID-font stream the external worker owns.
		if textiness(inflated) < minStreamTextiness || wordiness(inflated) < minWordiness {
			continue
		}
		out.WriteString(pdfTextOps(inflated))
	}
	res := strings.TrimSpace(out.String())
	// Final gate: a surviving mix of text and noise means the document is not
	// something this extractor can honestly represent. Return "" so the caller's
	// skip accounting records it (extract_failed/empty) instead of storing a
	// half-garbage source. Truncating to a good prefix is not an option: the
	// citation offsets would then point into a body that is not the document.
	if textiness(res) < minDocTextiness || wordiness(res) < minWordiness {
		return ""
	}
	// A watermark/boilerplate stamp repeated on every page passes both gates
	// above (it IS text) while carrying none of the document. One pirated-book
	// PDF extracted to 848 lines, 3 distinct, 99.8% the same URL spam.
	if topLineShare(res) > maxTopLineShare {
		return ""
	}
	return res
}

// maxTopLineShare is how much of a document one repeated line may occupy before
// we treat the extraction as boilerplate rather than content.
const maxTopLineShare = 0.5

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

// minWordiness is the share of non-space characters that must sit in a run of
// at least two. Measured on real corpora: 0.999-1.000. A CID/Type0-font PDF
// shatters one glyph per text-showing operator, so its extraction is one
// character per line: wordiness 0.42. textiness alone cannot see this — the
// characters are all letters — so both signals are required.
const minWordiness = 0.90

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

// minDocTextiness is the whole-document bar. It is looser than the per-stream
// bar because a real extraction legitimately mixes prose with a little binary
// cruft, but a half-garbage document must not become a searchable source.
const minDocTextiness = 0.90

// minStreamTextiness is the share of a stream that must be text-like before we
// scrape text operators out of it. Measured: real prose 1.00, law/baike/poetry
// corpora 0.99-1.00, embedded font programs and image data 0.30-0.55.
const minStreamTextiness = 0.85

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

func pdfTextOps(s string) string {
	var b strings.Builder
	for _, lit := range pdfTextRe.FindAllString(s, -1) {
		inner := lit[1 : len(lit)-1]
		if pdfTextRe.MatchString(inner) { // avoid nested misuse
			continue
		}
		for _, r := range pdfUnescape(inner) {
			if unicode.IsPrint(r) || r == ' ' || unicode.IsSpace(r) {
				b.WriteRune(r)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func pdfUnescape(s string) string {
	r := strings.NewReplacer(
		`\n`, "\n", `\r`, "\r", `\t`, "\t",
		`\(`, "(", `\)`, ")", `\\`, `\`,
	)
	return r.Replace(s)
}
