package ingest

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"strings"
	"testing"
)

func TestExtractHTML(t *testing.T) {
	raw := `<html><head><style>body{color:red}</style>` +
		`<script>var x = 1;</script></head>` +
		`<body><h1>部署手册</h1><p>连接池最大 &amp; 超时 30&nbsp;秒。</p>` +
		`<div>无关段落</div><!-- 注释 --></body></html>`
	text := ExtractHTML(raw)
	for _, bad := range []string{"<", ">", "color", "var x", "注释"} {
		if strings.Contains(text, bad) {
			t.Fatalf("html/extraction residue %q in: %q", bad, text)
		}
	}
	if !strings.Contains(text, "部署手册") || !strings.Contains(text, "连接池最大 & 超时 30 秒。") {
		t.Fatalf("content lost: %q", text)
	}
	if strings.Contains(text, "&amp;") {
		t.Fatalf("entity not decoded: %q", text)
	}
}

func TestExtractHTMLBlockBreaks(t *testing.T) {
	text := ExtractHTML("<p>一</p><p>二</p>")
	if !strings.Contains(text, "一") || !strings.Contains(text, "二") {
		t.Fatalf("paragraph text lost: %q", text)
	}
	if strings.Contains(text, "一二") {
		t.Fatalf("block paragraphs merged without break: %q", text)
	}
}

func docxFixture(t *testing.T, documentXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(documentXML)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractDOCX(t *testing.T) {
	raw := docxFixture(t, `<w:document><w:body>`+
		`<w:p><w:r><w:t>连接池最大 128。</w:t></w:r></w:p>`+
		`<w:p><w:r><w:t>部署在广州机房。</w:t></w:r></w:p>`+
		`</w:body></w:document>`)
	text, err := ExtractDOCX(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "连接池最大 128。") || !strings.Contains(text, "部署在广州机房。") {
		t.Fatalf("docx paragraphs lost: %q", text)
	}
	if _, err := ExtractDOCX([]byte("not a zip")); err == nil {
		t.Fatal("non-zip must error")
	}
}

func TestExtractPDFPlainAndFlate(t *testing.T) {
	// Uncompressed content stream (Tj + TJ array).
	plain := []byte("BT /F1 12 Tf (Hello world) Tj [(\\(esc\\))] TJ ET")
	// Flate-compressed content stream.
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write([]byte("BT (compressed text ok) Tj ET")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	pdf := append([]byte("stream\n"), plain...)
	pdf = append(pdf, []byte("\nendstream\nstream\n")...)
	pdf = append(pdf, zbuf.Bytes()...)
	pdf = append(pdf, []byte("\nendstream\n%%EOF")...)
	text := ExtractPDF(pdf)
	if !strings.Contains(text, "Hello world") || !strings.Contains(text, "compressed text ok") {
		t.Fatalf("both streams must be pulled: %q", text)
	}
	if !strings.Contains(text, "(esc)") {
		t.Fatalf("escaped parens must decode: %q", text)
	}
	if strings.Contains(text, "BT") || strings.Contains(text, "Tj") {
		t.Fatalf("pdf operators leaked into body: %q", text)
	}
}
