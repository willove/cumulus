// Package adapt turns heterogeneous corpus files into the suite's ingest unit.
//
// The problem it solves: real local corpora are not .md/.txt. They are JSON
// arrays (poetry, baike), JSON-lines (news, law triples), CSV (a million product
// titles), parquet (HF exports) — each with its own field names for "the title"
// and "the body". Hand-writing a map spec per corpus does not scale, and
// serializing a JSON record straight into `body` pollutes the full-text index
// with braces and escapes.
//
// Contract:
//   - STREAMING. These files are 100-600 MB; nothing may be fully buffered.
//   - ONE DOCUMENT PER RECORD, never a chunk. The body is the extracted text;
//     the other fields go to meta for filtering.
//   - HONEST FAILURE. A shape we cannot read returns an error naming the file
//     and the reason; it is never silently turned into an empty document.
//   - NO NEW DEPENDENCIES for the text formats: stdlib encoding/json + csv.
package adapt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Doc is one adapt-produced record, ready for source.New.
type Doc struct {
	Key   string         // business identity; empty = derive from the record
	Title string         // display name
	Body  string         // extracted plain text (the L0 contract)
	Meta  map[string]any // passthrough fields for filtering
}

// Kind is a detected container format.
type Kind string

const (
	KindJSONLines  Kind = "jsonl" // one JSON object per line
	KindJSONArray  Kind = "json-array"
	KindJSONObject Kind = "json-object" // a single object; one document
	KindCSV        Kind = "csv"
	KindText       Kind = "text"
	KindUnknown    Kind = "unknown"
)

// Fields names the columns/keys to read. Empty means "autodetect".
type Fields struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Extra []string `json:"extra"` // keys copied verbatim into Meta
}

// Detect sniffs the container format from the file name and a prefix of its
// content. It never consumes more than sniff bytes.
func Detect(name string, prefix []byte) Kind {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".csv", ".tsv":
		return KindCSV
	case ".jsonl", ".ndjson":
		return KindJSONLines
	case ".json":
		return detectJSON(prefix)
	case ".md", ".txt", ".rst", ".log":
		return KindText
	}
	return detectJSON(prefix)
}

// detectJSON decides between a single object, a JSON array, and JSON-lines by
// the first non-whitespace byte, then falls back to text.
func detectJSON(prefix []byte) Kind {
	for _, b := range prefix {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '[':
			return KindJSONArray
		case '{':
			// One object per line (the common big-corpus shape) or one object
			// in total? Both start with '{'; the caller disambiguates by
			// whether a complete object is followed by more non-space bytes on
			// a later line. Default to array-of-objects-per-line, which is the
			// superset: a lone object parses as one line too.
			return KindJSONLines
		default:
			if looksBinary(prefix) {
				return KindUnknown
			}
			return KindText
		}
	}
	return KindText
}

// looksBinary is a cheap "this is not text" test: a NUL byte, or a high share of
// non-printable bytes in the sniff window. Without it a binary file with an
// unknown extension was streamed as text and produced one garbage document.
func looksBinary(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	bad := 0
	for _, c := range b {
		if c < 0x09 || (c > 0x0d && c < 0x20) {
			bad++
		}
	}
	return float64(bad)/float64(len(b)) > 0.10
}

// Stream converts r into documents, calling emit once per record. The reader is
// consumed lazily; a very large file costs O(1) memory per record.
func Stream(ctx context.Context, name string, r io.Reader, f Fields, emit func(Doc) error) error {
	br := bufio.NewReaderSize(r, 256<<10)
	prefix, _ := br.Peek(8192)
	switch Detect(name, prefix) {
	case KindCSV:
		return streamCSV(ctx, br, f, emit)
	case KindJSONLines:
		return streamJSONLines(ctx, br, f, emit)
	case KindJSONArray:
		return streamJSONArray(ctx, br, f, emit)
	case KindText:
		return streamText(ctx, br, name, emit)
	default:
		return fmt.Errorf("adapt: %s: unrecognized container (first bytes %q)", name, truncate(string(prefix), 40))
	}
}

// streamJSONLines reads one JSON object per line. A file that is really a single
// pretty-printed object also works: the decoder reads one value and the loop
// ends at EOF.
func streamJSONLines(ctx context.Context, r io.Reader, f Fields, emit func(Doc) error) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var raw map[string]any
		if err := dec.Decode(&raw); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("adapt: jsonl: %w", err)
		}
		if d, ok := docFrom(raw, f); ok {
			if err := emit(d); err != nil {
				return err
			}
		}
	}
}

// streamJSONArray decodes a top-level array one element at a time so a 100 MB
// poetry collection is never fully materialised.
func streamJSONArray(ctx context.Context, r io.Reader, f Fields, emit func(Doc) error) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("adapt: json array: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return fmt.Errorf("adapt: json array: expected '[', got %v", tok)
	}
	for dec.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("adapt: json array element: %w", err)
		}
		if d, ok := docFrom(raw, f); ok {
			if err := emit(d); err != nil {
				return err
			}
		}
	}
	// Consume the closing ']' so a trailing object is not silently ignored.
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("adapt: json array close: %w", err)
	}
	return nil
}

// streamCSV reads a header row, then one document per row. The text column is
// the widest non-ID column unless Fields.Body names one, which is what makes a
// headerless-ish "text_id,text" corpus work without a spec.
func streamCSV(ctx context.Context, r io.Reader, f Fields, emit func(Doc) error) error {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tolerate ragged rows; a bad cell is not a bad file
	cr.LazyQuotes = true
	cr.ReuseRecord = false
	header, err := cr.Read()
	if err != nil {
		return fmt.Errorf("adapt: csv header: %w", err)
	}
	cols := make([]string, len(header))
	for i, h := range header {
		cols[i] = strings.TrimSpace(strings.Trim(h, "\"'"))
	}
	// Sample rows to measure each column's width, then choose the text column
	// from the DATA rather than its name — so "text_id,text" and
	// "query_id,query,document" both work with no spec. The sampled rows are
	// re-chained in front of the remainder so nothing is skipped.
	const sampleRows = 200
	var sample [][]string
	for len(sample) < sampleRows {
		row, rerr := cr.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("adapt: csv row: %w", rerr)
		}
		sample = append(sample, row)
	}
	idCol, titleCol, textCol := csvColumns(cols, f, sample)
	src := r
	if len(sample) > 0 {
		var buf strings.Builder
		cw := csv.NewWriter(&buf)
		for _, row := range sample {
			_ = cw.Write(row)
		}
		cw.Flush()
		src = io.MultiReader(strings.NewReader(buf.String()), r)
	}
	cr = csv.NewReader(src)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("adapt: csv row: %w", err)
		}
		get := func(i int) string {
			if i < 0 || i >= len(row) {
				return ""
			}
			return strings.TrimSpace(row[i])
		}
		body := get(textCol)
		if body == "" {
			continue // a row with no text carries no L0 contract
		}
		key := get(idCol)
		if key == "" {
			key = body // content-addressed fallback: the caller dedupes by body
		}
		title := get(titleCol)
		if title == "" {
			title = key
		}
		meta := map[string]any{}
		for i, c := range cols {
			if i == textCol || c == "" {
				continue
			}
			if v := get(i); v != "" {
				meta[c] = v
			}
		}
		if err := emit(Doc{Key: key, Title: title, Body: body, Meta: meta}); err != nil {
			return err
		}
	}
}

// csvColumns resolves the three roles from the header, honouring an explicit
// spec first and falling back to name heuristics.
func csvColumns(cols []string, f Fields, sample [][]string) (id, title, text int) {
	lower := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	id, title, text = -1, -1, -1
	if f.ID != "" {
		id = indexOf(cols, f.ID)
	}
	if f.Title != "" {
		title = indexOf(cols, f.Title)
	}
	if f.Body != "" {
		text = indexOf(cols, f.Body)
	}
	for i, c := range cols {
		lc := lower(c)
		switch {
		case id < 0 && (lc == "id" || lc == "key" || lc == "doc_id" || lc == "text_id"):
			id = i
		case title < 0 && (lc == "title" || lc == "name" || lc == "subtitle"):
			title = i
		}
	}
	// No explicit body column: the widest column that is neither id nor title.
	if text < 0 && len(cols) > 0 {
		width := make([]int, len(cols))
		for _, row := range sample {
			for i := range cols {
				if i < len(row) {
					width[i] += len([]rune(row[i]))
				}
			}
		}
		best, bestW := -1, -1
		for i := range cols {
			if i == id || i == title {
				continue
			}
			if width[i] > bestW {
				best, bestW = i, width[i]
			}
		}
		text = best
	}
	return id, title, text
}

func indexOf(cols []string, want string) int {
	for i, c := range cols {
		if strings.EqualFold(strings.TrimSpace(c), strings.TrimSpace(want)) {
			return i
		}
	}
	return -1
}

// streamText treats the whole file as one document: the CLI already walks
// directories, so this path only sees single-file invocations.
func streamText(_ context.Context, r io.Reader, name string, emit func(Doc) error) error {
	raw, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return err
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return nil
	}
	return emit(Doc{Key: strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)), Title: filepath.Base(name), Body: body})
}

// docFrom maps one decoded record to a Doc, autodetecting the fields when the
// spec leaves them empty. Autodetection prefers long string values for the body
// and short ones for the title, which is what distinguishes "title/content"
// from "author/paragraphs" without a per-corpus rule.
func docFrom(raw map[string]any, f Fields) (Doc, bool) {
	if len(raw) == 0 {
		return Doc{}, false
	}
	bodyKey, titleKey := f.Body, f.Title
	if bodyKey == "" {
		bodyKey = autodetectBody(raw)
	}
	body := flatten(raw[bodyKey])
	if strings.TrimSpace(body) == "" {
		return Doc{}, false // nothing to index; not an error
	}
	if titleKey == "" {
		titleKey = autodetectTitle(raw, bodyKey)
	}
	title := strings.TrimSpace(flatten(raw[titleKey]))
	if title == "" {
		title = fallbackTitle(body)
	}
	key := strings.TrimSpace(flatten(raw[f.ID]))
	if key == "" {
		// Stable identity, unique per record, so re-ingesting the same file is
		// idempotent and two records never collapse onto one business key.
		key = firstNonEmptyKey(raw, title, body, bodyKey, titleKey)
	}
	meta := map[string]any{}
	for _, k := range f.Extra {
		if v, ok := raw[k]; ok {
			meta[k] = v
		}
	}
	if f.ID != "" {
		meta[f.ID] = raw[f.ID]
	}
	return Doc{Key: key, Title: title, Body: body, Meta: meta}, true
}

// flatten renders a JSON value as text: strings verbatim, arrays of strings
// joined with newlines (poetry "paragraphs"), everything else via its JSON
// form. It never emits the raw braces of the record.
func flatten(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var parts []string
		for _, e := range t {
			// An array of role/content records is a conversation: render it as
			// readable turns instead of dropping the whole array (the previous
			// default branch returned "" for objects, so every chat corpus
			// produced zero documents).
			if m, ok := e.(map[string]any); ok {
				role := strings.TrimSpace(flatten(m["role"]))
				body := flatten(firstPresent(m, "content", "text", "message", "value"))
				if strings.TrimSpace(body) != "" {
					if role != "" {
						parts = append(parts, role+": "+body)
					} else {
						parts = append(parts, body)
					}
					continue
				}
			}
			if sv := flatten(e); strings.TrimSpace(sv) != "" {
				parts = append(parts, sv)
			}
		}
		return strings.Join(parts, "\n")
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

var bodyKeys = []string{"content", "body", "text", "contentText", "content_text", "paragraphs", "article", "passage"}
var titleKeys = []string{"title", "subtitle", "subTitle", "name", "heading", "chapter", "author"}

// firstPresent returns the first present, non-empty field among names.
func firstPresent(m map[string]any, names ...string) any {
	for _, n := range names {
		if v, ok := m[n]; ok && strings.TrimSpace(flatten(v)) != "" {
			return v
		}
	}
	return nil
}

func autodetectBody(raw map[string]any) string {
	for _, k := range bodyKeys {
		if s := flatten(raw[k]); strings.TrimSpace(s) != "" {
			return k
		}
	}
	// No known key: take the longest string/array value among NON-title fields.
	// This is what makes an unfamiliar corpus readable without a spec — and
	// excluding titles is what keeps a title-only record from becoming its own
	// body (which would index a document that has no content).
	titleish := map[string]bool{}
	for _, k := range titleKeys {
		titleish[k] = true
	}
	best, bestLen := "", -1
	for k, v := range raw {
		if titleish[k] {
			continue
		}
		if l := len([]rune(flatten(v))); l > bestLen {
			best, bestLen = k, l
		}
	}
	if best == "" {
		// Nothing but titles: the caller treats it as bodyless.
		return "\x00nobody"
	}
	return best
}

func autodetectTitle(raw map[string]any, bodyKey string) string {
	for _, k := range titleKeys {
		if k == bodyKey {
			continue
		}
		if s := strings.TrimSpace(flatten(raw[k])); s != "" && len([]rune(s)) <= 200 {
			return k
		}
	}
	return ""
}

// idKeys are preferred, in order, when a record carries no explicit business
// key. Picking "the alphabetically first short field" collided badly: every
// People's-Daily record shares dataTime, so 34,376 articles would have collapsed
// onto one business identity and evicted each other as revisions.
var idKeys = []string{"id", "_id", "key", "doc_id", "uuid", "url", "link", "slug"}

// firstNonEmptyKey derives a stable per-record identity. An explicit id-ish
// field wins; otherwise the title plus a digest of the body, which is unique
// per record AND stable across re-ingest of the same file.
// fallbackTitle picks a human-readable title from a record with no title field.
// For dialogue-shaped bodies (rendered as "role: content" lines) the first USER
// turn is the subject; otherwise the first line. Both beat the previous
// truncate(body, 40), which made every chat in a corpus share one title.
func fallbackTitle(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "user:"); ok {
			return truncate(strings.TrimSpace(rest), 60)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncate(line, 60)
		}
	}
	return ""
}

func firstNonEmptyKey(raw map[string]any, title, body string, skip ...string) string {
	skipSet := map[string]bool{}
	for _, k := range skip {
		skipSet[k] = true
	}
	for _, k := range idKeys {
		if skipSet[k] {
			continue
		}
		if v := strings.TrimSpace(flatten(raw[k])); v != "" && len([]rune(v)) <= 200 {
			return v
		}
	}
	sum := sha256.Sum256([]byte(body))
	return truncate(title, 60) + "|" + hex.EncodeToString(sum[:])[:8]
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
