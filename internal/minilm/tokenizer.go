package minilm

// The tokenizer replicates the HF tokenizers pipeline recorded in
// tokenizer.json: Precompiled-charsmap normalization (dumped to
// normalization_map.json as a per-rune table — 4806 non-identity entries),
// WhitespaceSplit + Metaspace("▁", add_prefix_space), Unigram Viterbi with
// <unk>=3, then the "<s> A </s>" template with a 128-token cap.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
)

const (
	bosID = 0
	padID = 1 // not used by encode, kept for spec completeness
	eosID = 2
	unkID = 3

	maxSeqTokens = 128 // sentence_bert_config.json max_seq_length
	unkPenalty   = 10.0
	metaspace    = "▁"
)

// Tokenizer is the frozen HF spec of the model's tokenizer.
type Tokenizer struct {
	score   map[string]float64 // piece → unigram score
	id      map[string]int     // piece → vocab id
	maxRune int                // longest piece in runes
	minScr  float64
	normMap map[rune]string // charsmap non-identity entries; identity default
}

//go:embed normalization_map.json
var normMapJSON []byte

// LoadTokenizer reads unigram.json ({"unk_id":3,"vocab":[[piece,score],…]}) from
// the model dir; the precompiled-charsmap normalization table ships embedded.
func LoadTokenizer(dir string) (*Tokenizer, error) {
	raw, err := os.ReadFile(dir + "/unigram.json")
	if err != nil {
		return nil, err
	}
	var file struct {
		UnkID int      `json:"unk_id"`
		Vocab [][2]any `json:"vocab"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("unigram.json: %w", err)
	}
	if file.UnkID != unkID {
		return nil, fmt.Errorf("unigram.json: unk_id %d, want %d", file.UnkID, unkID)
	}
	vocab := file.Vocab
	t := &Tokenizer{
		score:   make(map[string]float64, len(vocab)),
		id:      make(map[string]int, len(vocab)),
		minScr:  1e9,
		normMap: map[rune]string{},
	}
	for _, e := range vocab {
		piece, ok := e[0].(string)
		if !ok {
			return nil, fmt.Errorf("unigram.json: non-string piece at %d", len(t.id))
		}
		var score float64
		switch v := e[1].(type) {
		case float64:
			score = v
		case string:
			if _, err := fmt.Sscan(v, &score); err != nil {
				return nil, fmt.Errorf("unigram.json: score for %q: %w", piece, err)
			}
		default:
			return nil, fmt.Errorf("unigram.json: bad score for %q", piece)
		}
		id := len(t.id)
		t.score[piece] = score
		t.id[piece] = id
		if score < t.minScr {
			t.minScr = score
		}
		if n := len([]rune(piece)); n > t.maxRune {
			t.maxRune = n
		}
	}
	var hexmap map[string]string
	if err := json.Unmarshal(normMapJSON, &hexmap); err != nil {
		return nil, fmt.Errorf("normalization_map.json: %w", err)
	}
	for k, v := range hexmap {
		cp, err := strconv.ParseUint(strings.TrimPrefix(k, "0x"), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("normalization_map.json: key %q: %w", k, err)
		}
		t.normMap[rune(cp)] = v
	}
	return t, nil
}

// normalize applies the precompiled charsmap per rune (identity default).
func (t *Tokenizer) normalize(s string) string {
	need := false
	for _, r := range s {
		if _, ok := t.normMap[r]; ok {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if rep, ok := t.normMap[r]; ok {
			b.WriteString(rep)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// preTokenize = WhitespaceSplit (drop empties) + Metaspace prefix per piece.
func preTokenize(s string) []string {
	fields := strings.FieldsFunc(s, unicode.IsSpace)
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = metaspace + f
	}
	return out
}

type vstep struct {
	start int32 // winning piece starts here (runes)
	id    int32 // winning vocab id (unkID on the unk path)
}

// Encode runs the full pipeline: normalize → pre-tokenize → Unigram Viterbi →
// "<s> A </s>" with the 128-token cap (specials included, matching
// SentenceTransformer truncation).
func (t *Tokenizer) Encode(text string) []int {
	ids := make([]int, 1, maxSeqTokens)
	ids[0] = bosID
	for _, piece := range preTokenize(t.normalize(text)) {
		ids = append(ids, t.viterbi(piece)...)
		if len(ids) >= maxSeqTokens-1 {
			break
		}
	}
	ids = append(ids, eosID)
	if len(ids) > maxSeqTokens {
		ids = ids[:maxSeqTokens]
		ids[len(ids)-1] = eosID
	}
	return ids
}

// viterbi segments one piece by max score-sum (HF Unigram). Ties prefer the
// longer piece: candidates relax longest-first with strict >.
func (t *Tokenizer) viterbi(piece string) []int {
	runes := []rune(piece)
	n := len(runes)
	unkScore := t.minScr - unkPenalty
	best := make([]float64, n+1)
	steps := make([]vstep, n+1)
	for i := 1; i <= n; i++ {
		best[i] = -1e18
		for l := minInt(i, t.maxRune); l >= 1; l-- {
			sub := string(runes[i-l : i])
			sc, ok := t.score[sub]
			if !ok {
				continue
			}
			if v := best[i-l] + sc; v > best[i] {
				best[i] = v
				steps[i] = vstep{start: int32(i - l), id: int32(t.id[sub])}
			}
		}
		// A single rune always has a path: <unk> with the HF penalty.
		if v := best[i-1] + unkScore; v > best[i] {
			best[i] = v
			steps[i] = vstep{start: int32(i - 1), id: unkID}
		}
	}
	out := make([]int, 0, n)
	for i := n; i > 0; {
		out = append(out, int(steps[i].id))
		i = int(steps[i].start)
	}
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
