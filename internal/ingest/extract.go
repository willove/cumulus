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
func ExtractPDF(raw []byte) string {
	var out strings.Builder
	for _, m := range pdfStreamRe.FindAllStringSubmatch(string(raw), -1) {
		data := m[1]
		plain := []byte(data)
		if zr, err := zlib.NewReader(strings.NewReader(data)); err == nil {
			if b, rerr := io.ReadAll(io.LimitReader(zr, 32<<20)); rerr == nil {
				plain = b
			}
			zr.Close()
		}
		out.WriteString(pdfTextOps(string(plain)))
	}
	return strings.TrimSpace(out.String())
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
