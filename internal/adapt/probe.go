package adapt

// Probing: what is this file, and what fields does it carry?
//
// This exists so the HTTP face and the workbench can show an operator the
// detected container and the available column/key names BEFORE committing to a
// mapping — instead of guessing, ingesting, and discovering from the search
// results that the wrong column became the body.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"os"
	"sort"
)

// Probe is one file's adaptation plan.
type Probe struct {
	Path     string   `json:"path"`
	Kind     Kind     `json:"kind"`
	Fields   []string `json:"fields"`   // available column/key names, sorted
	Bodyish  []string `json:"bodyish"`  // fields that look like document text
	Titleish []string `json:"titleish"` // fields that look like titles
	IDish    []string `json:"idish"`    // fields that look like identities
	Records  int      `json:"records"`  // sampled record count (0 = not sampled)
	Bytes    int64    `json:"bytes"`
	Note     string   `json:"note,omitempty"`
}

// ProbeFile inspects one file: container kind, the field names a mapper could
// use, and a small sample so the caller can also show a preview. It reads at
// most probeHeadBytes of the file, so probing a 600 MB corpus is cheap.
func ProbeFile(ctx context.Context, path string) (Probe, error) {
	fh, err := os.Open(path)
	if err != nil {
		return Probe{}, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return Probe{}, err
	}
	br := bufio.NewReaderSize(fh, 64<<10)
	prefix, _ := br.Peek(8)
	p := Probe{Path: path, Bytes: st.Size()}
	if looksLikeParquet(prefix) {
		return probeParquet(ctx, path, p)
	}
	// Read a bounded head for the text formats; the rest of the file is not
	// needed to know its shape.
	head := make([]byte, probeHeadBytes)
	n, _ := io.ReadFull(br, head)
	head = head[:n]
	p.Kind = Detect(path, head)
	switch p.Kind {
	case KindCSV:
		p.Fields, p.Note = probeCSV(head)
	case KindJSONLines:
		p.Fields, p.Records, p.Note = probeJSONLines(head)
	case KindJSONArray:
		p.Fields, p.Records, p.Note = probeJSONArray(head)
	case KindText:
		p.Note = "single text document"
	default:
		p.Note = "unrecognized container"
	}
	classifyFields(&p)
	return p, nil
}

// probeHeadBytes bounds how much of a file a probe reads.
const probeHeadBytes = 256 << 10

// probeParquet reads the schema only — it needs the path, hence its own branch.
func probeParquet(ctx context.Context, path string, p Probe) (Probe, error) {
	cols, rows, err := parquetSchema(ctx, path)
	if err != nil {
		return p, err
	}
	p.Kind = KindParquet
	p.Fields = cols
	p.Records = int(rows)
	p.Note = "columns discovered from the file schema (never assumed)"
	classifyFields(&p)
	return p, nil
}

func probeCSV(head []byte) ([]string, string) {
	cr := csv.NewReader(newByteReader(head))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	rec, err := cr.Read()
	if err != nil {
		return nil, "no header row"
	}
	out := make([]string, 0, len(rec))
	for _, c := range rec {
		if c = trimQuotes(c); c != "" {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, ""
}

func probeJSONLines(head []byte) ([]string, int, string) {
	seen := map[string]bool{}
	n := 0
	for _, line := range splitLines(head) {
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		n++
		for k := range m {
			seen[k] = true
		}
		if n >= 50 {
			break
		}
	}
	return sortedKeys(seen), n, ""
}

func probeJSONArray(head []byte) ([]string, int, string) {
	dec := json.NewDecoder(newByteReader(head))
	seen := map[string]bool{}
	n := 0
	tok, err := dec.Token()
	if err != nil {
		return nil, 0, "not a JSON array"
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, 0, "not a JSON array"
	}
	for dec.More() && n < 50 {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		n++
		for k := range m {
			seen[k] = true
		}
	}
	return sortedKeys(seen), n, ""
}

// classifyFields tags the discovered names with the role autodetect would give
// them, so a UI can pre-select sensible defaults.
func classifyFields(p *Probe) {
	p.Bodyish, p.Titleish, p.IDish = nil, nil, nil
	for _, f := range p.Fields {
		switch {
		case containsStr(bodyKeys, f):
			p.Bodyish = append(p.Bodyish, f)
		case containsStr(titleKeys, f):
			p.Titleish = append(p.Titleish, f)
		case containsStr(idKeys, f):
			p.IDish = append(p.IDish, f)
		}
	}
	if len(p.Bodyish) == 0 && len(p.Fields) > 0 {
		// Nothing recognised: the widest field wins at ingest time, and the
		// UI should say so rather than leaving the operator to guess.
		p.Note = appendNote(p.Note, "no recognised body field; ingest uses the widest column")
	}
}

func appendNote(note, add string) string {
	if note == "" {
		return add
	}
	return note + "; " + add
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func trimQuotes(s string) string {
	for len(s) > 0 && (s[0] == '"' || s[0] == '\'') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == '"' || s[len(s)-1] == '\'') {
		s = s[:len(s)-1]
	}
	return s
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// newByteReader adapts a byte slice for the csv/json decoders.
func newByteReader(b []byte) io.Reader { return bytes.NewReader(b) }
