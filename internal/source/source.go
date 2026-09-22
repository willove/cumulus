// Package source defines the ingest unit for the cognitive-search suite:
// one source document — full text (body) plus a locator map — never fixed
// chunks. Evidence windows are character ranges into body at query time.
package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	StatusActive  = "active"
	StatusStale   = "stale"
	StatusDeleted = "deleted"
)

// Span maps a character range of Body back to a human label (page, section).
type Span struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// Source is one L0 contract document in ask_sources.
type Source struct {
	ID          string         `json:"_id"`
	Body        string         `json:"body"`
	Title       string         `json:"title"`
	SourceType  string         `json:"source_type"`
	SourceURI   string         `json:"source_uri"`
	Digest      string         `json:"digest"`
	Structure   []Span         `json:"structure"`
	Meta        map[string]any `json:"meta"`
	Lang        string         `json:"lang"`
	Version     int            `json:"version"`
	Status      string         `json:"status"`
	IngestedAt  time.Time      `json:"ingested_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	BusinessKey string         `json:"business_key,omitempty"`
}

func Digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func IDFor(body string) string {
	return "src:" + Digest(body)[:16]
}

// Normalize collapses whitespace runs and strips NUL.
func Normalize(body string) string {
	body = strings.ReplaceAll(body, "\x00", "")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	var b strings.Builder
	b.Grow(len(body))
	prevSpace := false
	for _, r := range body {
		if r == '\n' {
			b.WriteRune(r)
			prevSpace = false
			continue
		}
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}

var headingRe = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+)$`)
var pageRe = regexp.MustCompile(`(?m)^---\s*page\s+(\d+)\s*---\s*$`)

// BuildStructure derives locator spans from markdown headings and page marks.
// Offsets are rune indices into body so they align with mcs sample windows.
func BuildStructure(body string) []Span {
	type mark struct {
		kind, label string
		at          int // rune index
	}
	byteToRune := func(b int) int {
		return len([]rune(body[:b]))
	}
	var marks []mark
	for _, m := range headingRe.FindAllStringSubmatchIndex(body, -1) {
		marks = append(marks, mark{kind: "heading", label: strings.TrimSpace(body[m[4]:m[5]]), at: byteToRune(m[0])})
	}
	for _, m := range pageRe.FindAllStringSubmatchIndex(body, -1) {
		marks = append(marks, mark{kind: "page", label: "p" + body[m[2]:m[3]], at: byteToRune(m[0])})
	}
	n := len([]rune(body))
	if len(marks) == 0 {
		if body == "" {
			return nil
		}
		return []Span{{Kind: "doc", Label: "body", Start: 0, End: n}}
	}
	for i := 1; i < len(marks); i++ {
		for j := i; j > 0 && marks[j].at < marks[j-1].at; j-- {
			marks[j], marks[j-1] = marks[j-1], marks[j]
		}
	}
	out := make([]Span, 0, len(marks))
	for i, m := range marks {
		end := n
		if i+1 < len(marks) {
			end = marks[i+1].at
		}
		out = append(out, Span{Kind: m.kind, Label: m.label, Start: m.at, End: end})
	}
	return out
}

// SliceSpan returns the body text covered by span i (rune offsets).
func SliceSpan(body string, spans []Span, i int) (string, error) {
	if i < 0 || i >= len(spans) {
		return "", fmt.Errorf("span index %d out of range [0,%d)", i, len(spans))
	}
	s := spans[i]
	runes := []rune(body)
	if s.Start < 0 || s.End > len(runes) || s.Start > s.End {
		return "", fmt.Errorf("span %d has invalid range [%d,%d) body_len=%d", i, s.Start, s.End, len(runes))
	}
	return string(runes[s.Start:s.End]), nil
}

func New(title, sourceType, uri, businessKey, lang, body string, meta map[string]any) Source {
	n := Normalize(body)
	now := time.Now().UTC()
	if meta == nil {
		meta = map[string]any{}
	}
	return Source{
		ID:          IDFor(n),
		Body:        n,
		Title:       title,
		SourceType:  sourceType,
		SourceURI:   uri,
		Digest:      Digest(n),
		Structure:   BuildStructure(n),
		Meta:        meta,
		Lang:        lang,
		Version:     1,
		Status:      StatusActive,
		IngestedAt:  now,
		UpdatedAt:   now,
		BusinessKey: businessKey,
	}
}
