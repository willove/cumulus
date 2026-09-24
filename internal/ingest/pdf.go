package ingest

// PDF text extraction.
//
// Two extractors, tried in order:
//
//  1. ledongthuc/pdf (pure Go) — parses the font/encoding machinery properly,
//     including ToUnicode CMaps, so real books come out as readable prose. It
//     replaced the hand-rolled regex scraper, which inflated every Flate stream
//     in the file (embedded fonts and images included) and scraped "(...)" out
//     of binary blobs.
//  2. the in-tree regex fallback, for the odd file the library rejects.
//
// Both are then measured by the same quality gates in extract.go: a stream (or
// a whole document) that is not mostly text, not mostly multi-character runs, or
// is one repeated line, yields "" so the caller's skip accounting records it
// instead of storing noise.

import (
	"io"
	"os"
	"strings"

	pdf "github.com/ledongthuc/pdf"
)

// ExtractPDF returns the plain text of a PDF, or "" when the document's text
// layer cannot be honestly recovered. Callers treat "" as "skipped file", never
// as an empty document.
func ExtractPDF(raw []byte) string {
	if text := pdfViaLibrary(raw); text != "" {
		if gated := gateText(text); gated != "" {
			return gated
		}
	}
	// The library refused or produced unusable text: try the in-tree fallback.
	// A temp file is needed because the library and the fallback both take a
	// reader; the bytes are already in memory, so the copy is cheap.
	if text := pdfViaRegex(raw); text != "" {
		if gated := gateText(text); gated != "" {
			return gated
		}
	}
	return ""
}

// pdfViaLibrary extracts with ledongthuc/pdf. It needs an io.ReaderAt + size,
// hence the temp file for a []byte input.
func pdfViaLibrary(raw []byte) string {
	tmp, err := os.CreateTemp("", "cumulus-pdf-*.pdf")
	if err != nil {
		return ""
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if _, err := tmp.Write(raw); err != nil {
		return ""
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	r, err := pdf.NewReader(tmp, int64(len(raw)))
	if err != nil {
		return ""
	}
	plain, err := r.GetPlainText()
	if err != nil {
		return ""
	}
	var b strings.Builder
	if _, err := io.Copy(&b, io.LimitReader(plain, 64<<20)); err != nil && b.Len() == 0 {
		return ""
	}
	return b.String()
}
