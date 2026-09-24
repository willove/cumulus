package ingest

// Candidate discovery (P9): before ingesting a large directory, the operator
// sees what WOULD be ingested — extension/size/freshness rules, oversized and
// empty files reported as skipped, a stratified cap — and may optionally rank
// the survivors for one query topic through an LLM (opt-in, injected Ranker).
// Scanning touches no store and no query-side tree: it is an ingest-adjacent,
// operator-triggered step — the one thing worth taking from Sirchmunk's
// DirectoryScanner (deep-dive §9), without its query-time form.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultMaxCandidateSize caps one file at 8 MiB: larger is reported as
	// oversized and skipped (evidence windows and MiniLM both degrade on it).
	DefaultMaxCandidateSize = 8 << 20
	// DefaultHeadlineBytes is how much of a text candidate the ranker sees.
	DefaultHeadlineBytes = 400
	// ingestableExts mirrors the ingest-files walk: the formats the pipeline
	// can actually extract.
	ingestableExts = ".md,.txt,.html,.htm,.docx,.pdf"
)

// Candidate is one discovered file with its rule-derived metadata. Headline
// is text-only; binary formats (docx/pdf) rank on path + size + age.
type Candidate struct {
	Path     string `json:"path"`
	Ext      string `json:"ext"`
	Size     int64  `json:"size"`
	ModTime  string `json:"mod_time"`
	AgeDays  int    `json:"age_days"`
	Headline string `json:"headline,omitempty"`
}

// ScanReport is one discovery pass: survivors plus skip accounting so the
// operator sees everything the walk dropped.
type ScanReport struct {
	Dir        string         `json:"dir"`
	Walked     int            `json:"walked"`
	Candidates []Candidate    `json:"candidates"`
	Skipped    map[string]int `json:"skipped"`
}

// ScanOptions configures one pass. Zero values mean "rule defaults".
type ScanOptions struct {
	Recursive  bool
	MaxSize    int64         // 0 = DefaultMaxCandidateSize
	NewerThan  time.Duration // 0 = off; older files are skipped
	Limit      int           // 0 = no cap; stratified sample down to Limit
	Extensions []string      // nil = the ingestable set
}

// Ranker orders candidate paths for one query topic (LLM, opt-in). Returning
// fewer paths than candidates is fine; unknown paths are ignored.
type Ranker func(ctx context.Context, query string, cands []Candidate) ([]string, error)

// ScanDir walks dir and applies the rules. Deterministic: candidates come
// out newest-first (ties by path), so two scans of an unchanged tree agree.
func ScanDir(dir string, opt ScanOptions) (ScanReport, error) {
	rep := ScanReport{Dir: dir, Skipped: map[string]int{}}
	if dir == "" {
		return rep, fmt.Errorf("scan: dir required")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return rep, err
	}
	if !info.IsDir() {
		return rep, fmt.Errorf("scan: %s is not a directory", dir)
	}
	maxSize := opt.MaxSize
	if maxSize <= 0 {
		maxSize = DefaultMaxCandidateSize
	}
	exts := opt.Extensions
	if len(exts) == 0 {
		exts = strings.Split(ingestableExts, ",")
	}
	extSet := map[string]bool{}
	for _, e := range exts {
		extSet[strings.ToLower(e)] = true
	}
	now := time.Now()

	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if p != dir && (!opt.Recursive || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		rep.Walked++
		name := d.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
			rep.Skipped["hidden"]++
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if !extSet[ext] {
			rep.Skipped["ext"]++
			return nil
		}
		st, serr := d.Info()
		if serr != nil {
			return serr
		}
		if st.Size() == 0 {
			rep.Skipped["empty"]++
			return nil
		}
		if st.Size() > maxSize {
			rep.Skipped["oversized"]++
			return nil
		}
		age := now.Sub(st.ModTime())
		if opt.NewerThan > 0 && age > opt.NewerThan {
			rep.Skipped["old"]++
			return nil
		}
		rep.Candidates = append(rep.Candidates, Candidate{
			Path:     p,
			Ext:      ext,
			Size:     st.Size(),
			ModTime:  st.ModTime().UTC().Format(time.RFC3339),
			AgeDays:  int(age.Hours() / 24),
			Headline: headline(p, ext),
		})
		return nil
	})
	if err != nil {
		return rep, err
	}
	// Newest first, ties by path — deterministic without a sort flag.
	sort.Slice(rep.Candidates, func(i, j int) bool {
		a, b := rep.Candidates[i], rep.Candidates[j]
		if a.ModTime != b.ModTime {
			return a.ModTime > b.ModTime
		}
		return a.Path < b.Path
	})
	rep.Candidates = Stratify(rep.Candidates, opt.Limit)
	return rep, nil
}

// headline lifts the first non-empty line of a text candidate; binary
// formats stay empty (the ranker falls back to path/size/age).
func headline(p, ext string) string {
	switch ext {
	case ".md", ".txt", ".html", ".htm":
	default:
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), DefaultHeadlineBytes*4)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			// Rune-safe truncation: slicing bytes could split a multi-byte
			// character and put invalid UTF-8 in the candidate report.
			if r := []rune(line); len(r) > DefaultHeadlineBytes {
				return string(r[:DefaultHeadlineBytes])
			}
			return line
		}
	}
	return ""
}

// Stratify caps a rule-ordered candidate list at limit without letting one
// extension (or directory) sweep the budget: buckets by extension, then
// round-robins. Deterministic — same input, same output.
func Stratify(cands []Candidate, limit int) []Candidate {
	if limit <= 0 || len(cands) <= limit {
		return cands
	}
	buckets := map[string][]Candidate{}
	var order []string
	for _, c := range cands {
		if _, seen := buckets[c.Ext]; !seen {
			order = append(order, c.Ext)
		}
		buckets[c.Ext] = append(buckets[c.Ext], c)
	}
	sort.Strings(order)
	out := make([]Candidate, 0, limit)
	for len(out) < limit {
		progressed := false
		for _, ext := range order {
			b := buckets[ext]
			if len(b) == 0 {
				continue
			}
			out = append(out, b[0])
			buckets[ext] = b[1:]
			progressed = true
			if len(out) == limit {
				break
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

// ApplyRank reorders candidates by an LLM topic ranking: ranked paths first
// (in rank order), then whatever the ranker did not mention (rule order).
// A nil ranker or an error leaves the rule order untouched — discovery must
// never fail because the model did.
func ApplyRank(ctx context.Context, rep *ScanReport, query string, ranker Ranker) error {
	if ranker == nil || query == "" || len(rep.Candidates) == 0 {
		return nil
	}
	ranked, err := ranker(ctx, query, rep.Candidates)
	if err != nil {
		return err
	}
	byPath := map[string]Candidate{}
	for _, c := range rep.Candidates {
		byPath[c.Path] = c
	}
	seen := map[string]bool{}
	out := make([]Candidate, 0, len(rep.Candidates))
	for _, p := range ranked {
		if c, ok := byPath[p]; ok && !seen[p] {
			seen[p] = true
			out = append(out, c)
		}
	}
	for _, c := range rep.Candidates {
		if !seen[c.Path] {
			out = append(out, c)
		}
	}
	rep.Candidates = out
	return nil
}

// CandidateFile is the operator-editable on-disk form: a scan report can be
// trimmed (drop rows, reorder) and fed back to ingest.
type CandidateFile struct {
	Dir        string      `json:"dir"`
	Candidates []Candidate `json:"candidates"`
}

// LoadCandidateFile reads a scan output (or a hand-written one). A bare JSON
// array of paths is also accepted for convenience.
func LoadCandidateFile(path string) (CandidateFile, error) {
	var out CandidateFile
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err == nil && len(out.Candidates) > 0 {
		return out, nil
	}
	var paths []string
	if err := json.Unmarshal(raw, &paths); err != nil {
		return out, fmt.Errorf("scan: %s: neither report nor path array: %w", path, err)
	}
	for _, p := range paths {
		if p = strings.TrimSpace(p); p != "" {
			out.Candidates = append(out.Candidates, Candidate{Path: p})
		}
	}
	return out, nil
}

// Paths is the candidate list as plain file paths for the ingest loop.
func (cf CandidateFile) Paths() []string {
	out := make([]string, 0, len(cf.Candidates))
	for _, c := range cf.Candidates {
		if c.Path != "" {
			out = append(out, c.Path)
		}
	}
	return out
}
