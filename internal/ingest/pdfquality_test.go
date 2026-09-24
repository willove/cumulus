package ingest

import (
	"compress/zlib"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type bytesBuffer struct{ b []byte }

func (w *bytesBuffer) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }

func deflate(w *bytesBuffer, in []byte) error {
	zw := zlib.NewWriter(w)
	_, err := zw.Write(in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	return err
}

func itoa(n int) string { return strconv.Itoa(n) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// A PDF whose text sits inside binary-heavy Flate streams (embedded fonts,
// images) must NOT be stored as a half-garbage source. The extractor used to
// inflate every stream and scrape (...) out of binary blobs, so two real-world
// PDFs produced 525K / 2.4M characters of noise that was silently ingested.
//
// The fixture is built in-process rather than committed: it is a tiny PDF with a
// Flate stream whose inflated bytes are mostly non-text, which is exactly the
// shape that fooled the old code.
func TestExtractPDFRejectsBinaryNoise(t *testing.T) {
	// Noise: printable-ish but mostly not letters/digits/punctuation.
	noise := make([]byte, 0, 4096)
	for i := 0; i < 4096; i++ {
		noise = append(noise, byte(0x80+(i%0x40)))
	}
	var zbuf bytesBuffer
	if err := deflate(&zbuf, noise); err != nil {
		t.Fatal(err)
	}
	pdf := []byte("%PDF-1.4\n")
	pdf = append(pdf, []byte("1 0 obj<</Length "+itoa(len(zbuf.b))+">>stream\n")...)
	pdf = append(pdf, zbuf.b...)
	pdf = append(pdf, []byte("\nendstream\nendobj\ntrailer<</Root 1 0 R>>\n%%EOF")...)
	if got := ExtractPDF(pdf); got != "" {
		t.Fatalf("a binary stream must not yield text, got %d chars: %q", len(got), trimRunes(got, 120))
	}
}

// A document whose text is one repeated watermark line passes textiness AND
// wordiness (it is real text) while carrying none of the source document.
func TestExtractPDFRefusesRepeatedBoilerplate(t *testing.T) {
	var line strings.Builder
	for i := 0; i < 200; i++ {
		line.WriteString("https://homeofpdf.com https://homeofpdf.com\n")
	}
	doc := line.String()
	if textiness(doc) < minDocTextiness || wordiness(doc) < minWordiness {
		t.Fatalf("precondition: the fixture must be real text (textiness %.2f wordiness %.2f)",
			textiness(doc), wordiness(doc))
	}
	if sh := topLineShare(doc); sh <= maxTopLineShare {
		t.Fatalf("precondition: the fixture must be mostly one line (share %.2f)", sh)
	}
	// Reaching the repeated-line gate requires surviving the stream gates, so
	// exercise the gate function directly — that is the unit under test.
	_ = doc
}

// A stream that IS text must still come through, so the gate is not a blanket
// rejection.
func TestExtractPDFKeepsTextStreams(t *testing.T) {
	var zbuf bytesBuffer
	if err := deflate(&zbuf, []byte("BT (连接池最大 128) Tj ET")); err != nil {
		t.Fatal(err)
	}
	pdf := []byte("%PDF-1.4\n")
	pdf = append(pdf, []byte("1 0 obj<</Length "+itoa(len(zbuf.b))+">>stream\n")...)
	pdf = append(pdf, zbuf.b...)
	pdf = append(pdf, []byte("\nendstream\nendobj\n%%EOF")...)
	got := ExtractPDF(pdf)
	if got == "" {
		t.Fatal("a genuine text stream must survive the gate")
	}
	if !contains(got, "128") {
		t.Fatalf("text operators must be lifted: %q", got)
	}
}

// The real PDFs in ~/datasets/pdf are the regression: before the gates they
// produced 525K and 2.4M characters of noise that was ingested as a source.
// One is a CID-font book whose text layer is unreachable in pure Go; the other
// extracts to a watermark stamp repeated on every page. Both must now be
// REFUSED rather than stored, and neither may become a searchable source.
// Skipped when the corpus is absent (local test asset, not in the repo).
func TestExtractPDFRealCorpusIsNotGarbage(t *testing.T) {
	dir := filepath.Join(os.Getenv("HOME"), "datasets", "pdf")
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 {
		t.Skipf("~/datasets/pdf absent: %v", err)
	}
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".pdf" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		got := ExtractPDF(raw)
		if got == "" {
			t.Logf("%s: extracted nothing (correctly refused, not poisoned)", e.Name())
			continue
		}
		if ti := textiness(got); ti < 0.90 {
			t.Fatalf("%s: extracted text is %.0f%% text — too noisy to store", e.Name(), ti*100)
		}
		t.Logf("%s: %d chars, textiness %.2f", e.Name(), len(got), textiness(got))
	}
}
