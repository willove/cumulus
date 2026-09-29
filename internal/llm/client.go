// Package llm adapts chat-completions endpoints (aigate / OpenAI-compatible)
// to cumulus-cluster's Scorer and Embedder interfaces. Offline KeywordScorer/Local stay
// the gate carrier; this package is the production path (S5 plan D6).
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/prompts"
)

// ChatClient posts OpenAI-style chat completions. Endpoint config is
// per-suite (env/.env, internal integration) while the unified gateway is a
// future work item — aigate stays locked for now.
type ChatClient struct {
	BaseURL    string
	APIKey     string
	Model      string
	Caller     string
	HTTPClient *http.Client
	// ReasoningSplit asks a MiniMax-style endpoint to split chain-of-thought
	// into message.reasoning_content (content stays the clean answer).
	ReasoningSplit bool

	total atomic.Int64 // cumulative upstream-reported tokens
	// prompt/completion split, when the endpoint reports it (OpenAI 兼容面都给；
	// 缺席时保持 0，消费台账只记 total)。Atomic 与 total 同理。
	prompt     atomic.Int64
	completion atomic.Int64
}

func (c *ChatClient) http() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// Complete sends one chat turn and returns the assistant content.
func (c *ChatClient) Complete(ctx context.Context, user string) (string, error) {
	return c.complete(ctx, user, false)
}

// CompleteStructured sends one chat turn with the model's private thinking
// disabled, for mechanical extraction passes (keyword analysis, structured
// JSON out) whose reasoning is either discarded or re-derived later.
// Measured on MiniMax-M3 with the real fast_analyze prompt: thinking on
// ≈4.5-5.2s for ≈240 completion tokens of reasoning that nothing reads;
// thinking off ≈2.0s for ≈50 tokens of pure output, with keyword quality
// equal or cleaner. The synthesis call keeps thinking — there the reasoning
// IS the answer quality. Servers that don't know the field ignore it, same
// contract as reasoning_split.
func (c *ChatClient) CompleteStructured(ctx context.Context, user string) (string, error) {
	return c.complete(ctx, user, true)
}

func (c *ChatClient) complete(ctx context.Context, user string, noThink bool) (string, error) {
	if c.BaseURL == "" {
		return "", fmt.Errorf("llm: BaseURL required")
	}
	body := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "user", "content": user},
		},
		"temperature": 0,
	}
	if c.ReasoningSplit {
		// MiniMax-style opt-in: thinking lands in message.reasoning_content,
		// content stays the clean answer. Servers that don't know the field
		// ignore it.
		body["reasoning_split"] = true
	}
	if noThink {
		// MiniMax M-series accepts thinking{type:disabled}; gateways that
		// don't know it drop the unknown field.
		body["thinking"] = map[string]string{"type": "disabled"}
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Caller != "" {
		req.Header.Set("X-Aigate-Caller", c.Caller)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("llm: status %d: %s", resp.StatusCode, truncate(string(payload), 200))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens      int64 `json:"total_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", err
	}
	if out.Usage.TotalTokens > 0 {
		c.total.Add(out.Usage.TotalTokens) // budget accounting
	}
	if out.Usage.PromptTokens > 0 {
		c.prompt.Add(out.Usage.PromptTokens)
	}
	if out.Usage.CompletionTokens > 0 {
		c.completion.Add(out.Usage.CompletionTokens)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: empty choices")
	}
	// Models that inline their chain of thought (MiniMax-M3 without
	// reasoning_split) must not leak it into the answer: downstream prompts
	// expect parseable JSON, and think text with braces corrupts extraction.
	content, _ := SplitThink(out.Choices[0].Message.Content)
	if strings.TrimSpace(content) == "" {
		content = strings.TrimSpace(out.Choices[0].Message.ReasoningContent)
	}
	return content, nil
}

// TotalTokens reports the cumulative upstream-reported token usage of every
// completed call through this client (per-query accounting; the CLI reads
// it after one search). Atomic — serve handlers may share the client.
// CompleteStream sends one chat turn with stream=true and delivers each
// content delta to onDelta as it arrives, returning the full content. The
// synthesize face uses it so the user watches the answer being written
// instead of staring at a spinner for the whole generation. Transport or
// HTTP errors abort the stream and return the error; callers fall back to
// the non-streaming path.
func (c *ChatClient) CompleteStream(ctx context.Context, user string, onDelta func(string)) (string, error) {
	return c.completeStream(ctx, user, onDelta, "")
}

// CompleteStreamField is CompleteStream with a JSON field scrubber: only the
// named string field's body (unescaped) reaches onDelta, so a JSON-emitting
// model streams its summary text rather than its envelope.
func (c *ChatClient) CompleteStreamField(ctx context.Context, user, field string, onDelta func(string)) (string, error) {
	return c.completeStream(ctx, user, onDelta, field)
}

func (c *ChatClient) completeStream(ctx context.Context, user string, onDelta func(string), field string) (string, error) {
	if c.BaseURL == "" {
		return "", fmt.Errorf("llm: BaseURL required")
	}
	if onDelta == nil {
		onDelta = func(string) {}
	}
	var js *jsonFieldScrubber
	if field != "" {
		js = newJSONFieldScrubber(field)
	}
	body := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "user", "content": user},
		},
		"temperature": 0,
		"stream":      true,
	}
	if c.ReasoningSplit {
		body["reasoning_split"] = true
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Caller != "" {
		req.Header.Set("X-Aigate-Caller", c.Caller)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", fmt.Errorf("llm: status %d: %s", resp.StatusCode, truncate(string(payload), 200))
	}
	var full strings.Builder
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 64<<20))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20) // legal-evidence summaries can be long
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				break
			}
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // keep-alive comments / partial frames are not fatal
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content == "" {
				continue
			}
			full.WriteString(ch.Delta.Content)
			if js == nil {
				onDelta(ch.Delta.Content)
				continue
			}
			// Field scrubber: forward only the named field's body so the
			// browser renders the summary itself, not the JSON envelope.
			for _, out := range scrubFeed(js, ch.Delta.Content) {
				if out != "" {
					onDelta(out)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return full.String(), err
	}
	return full.String(), nil
}

func (c *ChatClient) TotalTokens() int64 { return c.total.Load() }

// SnapshotUsage 报告本 client 生命周期内的累计用量拆分。栈是每请求重建的，
// 所以一次检索结束时快照即该次的用量（消费台账按此记账）。
func (c *ChatClient) SnapshotUsage() (prompt, completion, total int64) {
	return c.prompt.Load(), c.completion.Load(), c.total.Load()
}

// thinkRe matches an inline chain-of-thought block.
var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

// SplitThink separates an inline <think>…</think> block from the answer.
// Unbalanced or absent blocks return the input unchanged.
func SplitThink(content string) (clean, reasoning string) {
	if m := thinkRe.FindStringIndex(content); m != nil {
		return strings.TrimSpace(content[m[1]:]), strings.TrimSpace(thinkRe.FindString(content))
	}
	return content, ""
}

// SynthesizeStream is the streaming twin of Synthesize: the same prompt and
// the same parse/repair, but the summary reaches the caller delta by delta.
// It does NOT fall back internally — a transport failure or an unparseable
// stream returns an error and the engine re-routes to the non-streaming
// Synthesize (no-think-first + thinking retry). The caller is responsible
// for reconciling partial deltas already on screen with the replacement
// summary (see searchapi's `replace` content event).
func (s *AigateSynthesizer) SynthesizeStream(ctx context.Context, query string, samples []mcs.Sample, onDelta func(string)) (string, error) {
	var ev strings.Builder
	for i, sm := range samples {
		fmt.Fprintf(&ev, "[%d] (%s [%d,%d)) %s\n", i+1, sm.Source, sm.Start, sm.End, truncateRunes(sm.Content, maxSampleRunes))
	}
	tmpl := prompts.MustRender(prompts.SynthesizeROI, map[string]string{
		"query":     query,
		"evidences": ev.String(),
	})
	raw, err := s.Client.CompleteStreamField(ctx, tmpl, "summary", onDelta)
	if err != nil {
		return "", err
	}
	out, perr := ParseSynthesizeJSON(raw)
	if perr != nil {
		return "", perr
	}
	if strings.TrimSpace(out.Summary) == "" {
		return "", fmt.Errorf("llm: empty summary")
	}
	s.setRefused(out.Refuse)
	return out.Summary, nil
}

// AigateScorer scores samples via the evaluate_sample prompt (v2: emits the
// per-fact oracle vector when given fact hints).
type AigateScorer struct {
	Client *ChatClient
	// NoThink routes evaluate_sample through CompleteStructured (private
	// thinking disabled).
	//
	// The prompt's own rule (see CompleteStructured) says mechanical
	// structured-JSON passes should not pay for reasoning nothing reads, and
	// this is the highest-frequency call in the suite (~10 per cold query).
	// It is opt-in because "does thinking off change the 0-10 relevance
	// scale" is NOT the same measurement the rule was calibrated on
	// (that was fast_analyze keyword quality) — see cmd/scoreprobe, which is
	// the instrument that has to answer it before this default flips.
	NoThink bool
}

// EvaluateResult is the evaluate_sample v2 JSON shape.
type EvaluateResult struct {
	Score     float64  `json:"score"`
	Reasoning string   `json:"reasoning"`
	Covers    []string `json:"covers"`
}

// ParseEvaluateJSON is exported for frozen prompt regression tests.
func ParseEvaluateJSON(raw string) (EvaluateResult, error) {
	var parsed EvaluateResult
	if err := parseJSON(raw, &parsed); err != nil {
		return EvaluateResult{}, err
	}
	return parsed, nil
}

// ParseScoreJSON keeps the v1 shape (score + reasoning) for frozen tests.
func ParseScoreJSON(raw string) (float64, string, error) {
	r, err := ParseEvaluateJSON(raw)
	if err != nil {
		return 0, "", err
	}
	return r.Score, r.Reasoning, nil
}

// Score implements mcs.Scorer (0–10).
func (s *AigateScorer) Score(ctx context.Context, query string, sm mcs.Sample) (float64, string, error) {
	r, err := s.evaluate(ctx, query, nil, sm)
	if err != nil {
		return 0, "", err
	}
	return r.Score, r.Reasoning, nil
}

// ScoreWithFacts implements mcs.FactAware (oracle vector): covers are
// clamped to the given fact ids.
func (s *AigateScorer) ScoreWithFacts(ctx context.Context, query string, facts []string, sm mcs.Sample) (float64, string, []string, error) {
	r, err := s.evaluate(ctx, query, facts, sm)
	if err != nil {
		return 0, "", nil, err
	}
	return r.Score, r.Reasoning, clampCovers(r.Covers, facts), nil
}

// BatchItem is one window's verdict from the batched evaluate call.
type BatchItem struct {
	ID        string   `json:"id"`
	Score     float64  `json:"score"`
	Reasoning string   `json:"reasoning"`
	Covers    []string `json:"covers"`
}

// firstJSONArray returns the first bracket-balanced array span in s that is
// also VALID JSON, string-aware (brackets inside JSON strings don't count),
// scanning past balanced-but-non-JSON spans (Chinese prose decorates with
// [引用]/[注1] shapes that balance as arrays but parse as nothing). Models
// decorate batch answers the same way they decorate single objects — prose
// around the array, or a second copy.
func firstJSONArray(s string) string {
	for from := 0; from < len(s); {
		start, depth := -1, 0
		inStr, esc := false, false
		found := ""
		for i, r := range s[from:] {
			switch {
			case esc:
				esc = false
			case inStr:
				if r == '"' {
					inStr = false
				} else if r == '\\' {
					esc = true
				}
			case r == '"':
				inStr = true
			case r == '[':
				if depth == 0 {
					start = from + i
				}
				depth++
			case r == ']':
				if depth > 0 {
					depth--
					if depth == 0 && start >= 0 {
						found = s[start : from+i+1]
						break
					}
				}
			}
			if found != "" {
				break
			}
		}
		if found == "" {
			return ""
		}
		if json.Valid([]byte(found)) {
			return found
		}
		// Not JSON (a prose bracket pair): resume scanning just past its
		// opening bracket, in case the real array nests inside it.
		from = start + 1
	}
	return ""
}

// ParseEvaluateBatchJSON is exported for frozen prompt regression tests. It
// requires exactly one item per expected window id, in the S1..Sn labelling
// the prompt mandates — a short or padded array is an error, never a silent
// truncation (downstream gates read per-window scores positionally).
//
// It deliberately does NOT go through parseJSON: the shared jsonRe matches
// OBJECTS only (\{[\s\S]*\}), so an array handed to parseJSON yields the
// first embedded object and a spurious "cannot unmarshal object into []".
func ParseEvaluateBatchJSON(raw string, want int) ([]BatchItem, error) {
	if arr := firstJSONArray(raw); arr != "" {
		raw = arr
	}
	var parsed []BatchItem
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// Same one-shot repair chance parseJSON gives objects: legal-text
		// quotes and raw newlines inside string values.
		fixed := repairModelJSON(raw)
		if fixed == raw {
			return nil, err
		}
		if err2 := json.Unmarshal([]byte(fixed), &parsed); err2 != nil {
			return nil, err
		}
	}
	if len(parsed) != want {
		return nil, fmt.Errorf("llm: evaluate_batch: want %d items, got %d", want, len(parsed))
	}
	byID := make(map[string]BatchItem, len(parsed))
	for _, it := range parsed {
		byID[it.ID] = it
	}
	out := make([]BatchItem, want)
	for i := range want {
		id := fmt.Sprintf("S%d", i+1)
		it, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("llm: evaluate_batch: missing window %s", id)
		}
		out[i] = it
	}
	return out, nil
}

// ScoreBatch implements mcs.BatchScorer: every window of the round in ONE
// call (v3a — call granularity only; the per-window schema is the single-
// window one). Facts ride along exactly as in ScoreWithFacts, covers clamped
// to the given ids.
func (s *AigateScorer) ScoreBatch(ctx context.Context, query string, facts []string, samples []mcs.Sample) ([]mcs.BatchResult, error) {
	if len(samples) == 0 {
		return nil, nil
	}
	factsText := "（none）"
	if len(facts) > 0 {
		factsText = strings.Join(facts, "\n")
	}
	var wins strings.Builder
	for i, sm := range samples {
		fmt.Fprintf(&wins, "[S%d] (Source: %s [%d,%d))\n...%s...\n\n",
			i+1, sm.Source, sm.Start, sm.End, truncateRunes(sm.Content, maxSampleRunes))
	}
	tmpl := prompts.MustRender(prompts.EvaluateBatch, map[string]string{
		"query":   query,
		"count":   strconv.Itoa(len(samples)),
		"facts":   factsText,
		"windows": strings.TrimRight(wins.String(), "\n"),
	})
	raw, err := s.call(ctx, tmpl)
	if err != nil {
		return nil, err
	}
	items, err := ParseEvaluateBatchJSON(raw, len(samples))
	if err != nil {
		return nil, err
	}
	out := make([]mcs.BatchResult, len(items))
	for i, it := range items {
		out[i] = mcs.BatchResult{Score: it.Score, Reasoning: it.Reasoning, Covers: clampCovers(it.Covers, facts)}
	}
	return out, nil
}

// clampCovers restricts oracle annotations to the given fact ids — a window
// may only claim facts the decomposer actually proposed.
func clampCovers(covers, facts []string) []string {
	allowed := map[string]bool{}
	for _, f := range facts {
		if i := strings.Index(f, ":"); i > 0 {
			f = f[:i]
		}
		allowed[f] = true
	}
	var out []string
	for _, c := range covers {
		if allowed[c] {
			out = append(out, c)
		}
	}
	return out
}

// DimItem is the v3b batch item shape (evaluate_dims): the batch fields plus
// the decomposed dims and cross-evidence contradiction marks. Novelty and
// Support are parsed for schema honesty (a drifting prompt fails the frozen
// test, not silently) but nothing consumes them yet — they ride the prompt
// because the paper's decomposed judgement is the point of the shape.
type DimItem struct {
	ID            string   `json:"id"`
	Score         float64  `json:"score"`
	Reasoning     string   `json:"reasoning"`
	Covers        []string `json:"covers"`
	Novelty       float64  `json:"novelty"`
	Support       float64  `json:"support"`
	ConflictsWith []string `json:"conflicts_with"`
}

// ParseEvaluateDimsJSON is ParseEvaluateBatchJSON's sibling for the dims
// payload: same balanced-array extraction, count and id contract, one repair
// chance. Exported for frozen prompt regression tests.
func ParseEvaluateDimsJSON(raw string, want int) ([]DimItem, error) {
	if arr := firstJSONArray(raw); arr != "" {
		raw = arr
	}
	var parsed []DimItem
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		fixed := repairModelJSON(raw)
		if fixed == raw {
			return nil, err
		}
		if err2 := json.Unmarshal([]byte(fixed), &parsed); err2 != nil {
			return nil, err
		}
	}
	if len(parsed) != want {
		return nil, fmt.Errorf("llm: evaluate_dims: want %d items, got %d", want, len(parsed))
	}
	byID := make(map[string]DimItem, len(parsed))
	for _, it := range parsed {
		byID[it.ID] = it
	}
	out := make([]DimItem, want)
	for i := range want {
		id := fmt.Sprintf("S%d", i+1)
		it, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("llm: evaluate_dims: missing window %s", id)
		}
		out[i] = it
	}
	return out, nil
}

// digestTop and maxDigestRunes bound the Current Evidence Digest: enough of
// the kept windows for the scorer to see what it might contradict, without
// the digest doubling the prompt on a long crawl.
const (
	digestTop     = 3
	maxDigestRunes = 400
)

// ScoreBatchConflict implements mcs.ConflictBatchScorer (v3b c_d): the
// batched call also sees the top kept windows so contradiction marks are
// cross-file. ConflictsWith ids ride onto the samples verbatim (K-labels are
// digest entries, S-labels are batch peers — both are for the caller's stop
// gate, the sampler never resolves them).
func (s *AigateScorer) ScoreBatchConflict(ctx context.Context, query string, facts []string, samples, prior []mcs.Sample) ([]mcs.BatchResult, error) {
	if len(samples) == 0 {
		return nil, nil
	}
	factsText := "（none）"
	if len(facts) > 0 {
		factsText = strings.Join(facts, "\n")
	}
	// Digest: top-scoring kept windows first, stable by position.
	idx := make([]int, len(prior))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return prior[idx[a]].Score > prior[idx[b]].Score })
	if len(idx) > digestTop {
		idx = idx[:digestTop]
	}
	var dig strings.Builder
	for n, i := range idx {
		fmt.Fprintf(&dig, "[K%d] %s\n", n+1, truncateRunes(prior[i].Content, maxDigestRunes))
	}
	digest := strings.TrimRight(dig.String(), "\n")
	if digest == "" {
		digest = "（none）"
	}
	var wins strings.Builder
	for i, sm := range samples {
		fmt.Fprintf(&wins, "[S%d] (Source: %s [%d,%d))\n...%s...\n\n",
			i+1, sm.Source, sm.Start, sm.End, truncateRunes(sm.Content, maxSampleRunes))
	}
	tmpl := prompts.MustRender(prompts.EvaluateDims, map[string]string{
		"query":        query,
		"count":        strconv.Itoa(len(samples)),
		"facts":        factsText,
		"digest":       digest,
		"digest_count": strconv.Itoa(len(idx)),
		"windows":      strings.TrimRight(wins.String(), "\n"),
	})
	raw, err := s.call(ctx, tmpl)
	if err != nil {
		return nil, err
	}
	items, err := ParseEvaluateDimsJSON(raw, len(samples))
	if err != nil {
		return nil, err
	}
	out := make([]mcs.BatchResult, len(items))
	for i, it := range items {
		out[i] = mcs.BatchResult{
			Score: it.Score, Reasoning: it.Reasoning,
			Covers: clampCovers(it.Covers, facts), Conflicts: it.ConflictsWith,
		}
	}
	return out, nil
}

func (s *AigateScorer) evaluate(ctx context.Context, query string, facts []string, sm mcs.Sample) (EvaluateResult, error) {
	factsText := "（none）"
	if len(facts) > 0 {
		factsText = strings.Join(facts, "\n")
	}
	tmpl := prompts.MustRender(prompts.EvaluateSample, map[string]string{
		"query":          query,
		"sample_source":  sm.Source,
		"sample_content": truncateRunes(sm.Content, maxSampleRunes),
		"facts":          factsText,
	})
	raw, err := s.call(ctx, tmpl)
	if err != nil {
		return EvaluateResult{}, err
	}
	r, err := ParseEvaluateJSON(raw)
	if err != nil {
		return EvaluateResult{}, err
	}
	if r.Score < 0 {
		r.Score = 0
	}
	if r.Score > 10 {
		r.Score = 10
	}
	return r, nil
}

// call dispatches on NoThink. Kept as a method so Score and ScoreWithFacts can
// never drift apart on the thinking flag.
func (s *AigateScorer) call(ctx context.Context, tmpl string) (string, error) {
	if s.NoThink {
		return s.Client.CompleteStructured(ctx, tmpl)
	}
	return s.Client.Complete(ctx, tmpl)
}

// AigateEmbedder requests embeddings (OpenAI-compatible /embeddings).
type AigateEmbedder struct {
	BaseURL    string
	APIKey     string
	Model      string
	N          int
	HTTPClient *http.Client
}

func (e *AigateEmbedder) Dims() int {
	if e.N <= 0 {
		return 64
	}
	return e.N
}

// Embed implements cluster.Embedder.
func (e *AigateEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if e.BaseURL == "" {
		return nil, fmt.Errorf("llm: embed BaseURL required")
	}
	body := map[string]any{"model": e.Model, "input": texts}
	raw, _ := json.Marshal(body)
	hc := e.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(e.BaseURL, "/")+"/embeddings", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("llm: embed status %d: %s", resp.StatusCode, truncate(string(payload), 200))
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("llm: embed count %d want %d", len(out.Data), len(texts))
	}
	vecs := make([][]float64, len(out.Data))
	for i, d := range out.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

var (
	_ mcs.Scorer       = (*AigateScorer)(nil)
	_ cluster.Embedder = (*AigateEmbedder)(nil)
)

// AigateFactBuilder decomposes a query into atomic evidence requirements via
// the decompose_query prompt, replacing the deterministic heuristic
// facts.Build in production (perf-plan §6 / P1-4).
//
// Why this exists, measured: facts.Build yields K=1 for 124 of 136 real
// questions, and every one of its 12 K>1 splits is a miscut inside a book
// title, a defined term, or an enumeration. A miscut fact is a phantom
// requirement — unreachable, so NeedContinue never clears and the DEEP loop
// spends its whole budget on it.
//
// NoThink defaults to false here, unlike the other structured-JSON passes. That
// is deliberate and NOT an oversight: scoreprobe measured that turning private
// thinking off moves mid-band (3~7) decisions from 64% to 52% stable, and this
// call exists precisely to stop mid-band churn from deciding K. The extra call
// is 1 per query against ~10 scoring calls, so the budget is the right place to
// spend it. Flip after a larger probe arm, not before.
type AigateFactBuilder struct {
	Client  *ChatClient
	NoThink bool
}

// Decompose implements facts.Decomposer.
func (b *AigateFactBuilder) Decompose(ctx context.Context, query string) ([]string, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	tmpl := prompts.MustRender(prompts.DecomposeQuery, map[string]string{"query": query})
	var raw string
	var err error
	if b.NoThink {
		raw, err = b.Client.CompleteStructured(ctx, tmpl)
	} else {
		raw, err = b.Client.Complete(ctx, tmpl)
	}
	if err != nil {
		return nil, err
	}
	return parseDecomposeJSON(raw, query)
}

// parseDecomposeJSON reads the prompt's JSON array of strings, then applies
// facts.BuildParts' semantic-unit gate. A model that echoes prose, returns an
// object, or emits fragments yields a nil slice — which the caller reads as
// "fall back to the heuristic", never as "search with no requirements".
func parseDecomposeJSON(raw, query string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	// Tolerate a fenced block, and a stray object wrapping the array.
	if i := strings.Index(raw, "["); i >= 0 {
		if j := strings.LastIndex(raw, "]"); j > i {
			raw = raw[i : j+1]
		}
	}
	var parts []string
	if err := json.Unmarshal([]byte(raw), &parts); err != nil {
		// Some runs answer with {"requirements": [...]}.
		var wrapper struct {
			Requirements []string `json:"requirements"`
			Facts        []string `json:"facts"`
			Parts        []string `json:"parts"`
		}
		if err2 := json.Unmarshal([]byte(raw), &wrapper); err2 != nil {
			return nil, nil // unreadable → heuristic, not an error the query pays for
		}
		parts = wrapper.Requirements
		if len(parts) == 0 {
			parts = wrapper.Facts
		}
		if len(parts) == 0 {
			parts = wrapper.Parts
		}
	}
	fx := facts.BuildParts(query, parts)
	out := make([]string, 0, len(fx))
	for _, f := range fx {
		out = append(out, f.Query)
	}
	return out, nil
}

// AigateAnalyzer classifies intent and extracts the keyword cascade via the
// fast_analyze prompt.
type AigateAnalyzer struct {
	Client *ChatClient
}

// Analyze implements fast.Analyzer.
//
// Runs thinking-disabled (CompleteStructured): this is mechanical keyword
// extraction whose reasoning nothing reads, and the measured cost on
// MiniMax-M3 with the real prompt is 4.95s with thinking vs 2.26s without.
// Intent classification and keywords came out equal-or-cleaner in the A/B.
//
// The one failure the no-think pass owns: on some runs it misclassifies a
// REAL question as chat/doc_summary (live sample: "什么情况下会被行政拘留"
// answered "（闲聊，不检索）" without any retrieval). Those verdicts skip
// retrieval entirely, so they are cross-checked against the deterministic
// gates in fast: a chat verdict for a query that is not greeting-shaped, or
// a doc_summary verdict for a query that is not a whole-document
// imperative, is re-run once with the thinking pass and that verdict wins.
// Genuine greetings pass the gate and never pay the extra call.
func (a *AigateAnalyzer) Analyze(ctx context.Context, query string) (fast.Analysis, error) {
	tmpl := prompts.MustRender(prompts.FastAnalyze, map[string]string{"query": query})
	raw, err := a.Client.CompleteStructured(ctx, tmpl)
	if err != nil {
		return fast.Analysis{}, err
	}
	an, err := ParseAnalyzeJSON(raw)
	if err != nil {
		// The model emitted unparseable JSON even after the balanced-object
		// extraction (observed on baike-058: trailing garbage). The cascade
		// still gets deterministic keywords from the rule analyzer — a lost
		// LLM weighting is cheap, a failed query is not (Search used to
		// return this error and cost the whole item).
		rule, _ := fast.RuleAnalyzer{}.Analyze(ctx, query)
		log.Printf("[llm] analyze unparseable (%v) — degraded to rule analyzer for %q", err, query)
		return rule, nil
	}
	if an.Intent == fast.IntentChat && !fast.LooksLikeChat(query) ||
		an.Intent == fast.IntentDocSummary && !fast.LooksLikeDocSummary(query) {
		raw2, err2 := a.Client.Complete(ctx, tmpl)
		if err2 == nil {
			if an2, perr := ParseAnalyzeJSON(raw2); perr == nil && an2.Intent != an.Intent {
				return an2, nil
			}
		}
		// The verify pass failed to parse or confirmed the fast verdict:
		// keep the fast one rather than failing the query.
	}
	return an, nil
}

// floatMap is an LLM-authored fact-weight map that tolerates the shapes a
// chat model actually emits: JSON numbers, numeric strings, and — from a
// creative model — non-numeric text under an invented key. baike-baseline
// item 058 died exactly there: the analyzer wrote the query term itself as
// a key with a phrase value, and map[string]float64 failed the whole
// unmarshal, costing the item. One skipped weight is cheap; one failed
// analysis is not.
type floatMap map[string]float64

func (m *floatMap) UnmarshalJSON(b []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		*m = nil // not an object: the cascade loses this level, not the query
		return nil
	}
	out := make(floatMap, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case float64:
			out[k] = t
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				out[k] = f
			}
		}
	}
	*m = out
	return nil
}

// AnalyzeResult is the fast_analyze JSON shape.
type AnalyzeResult struct {
	Intent   string   `json:"intent"`
	Primary  floatMap `json:"primary"`
	Fallback floatMap `json:"fallback"`
	Keywords floatMap `json:"keywords_alt"`
}

// firstJSONObject returns the first brace-balanced {...} span in s,
// string-aware (braces inside JSON strings don't count). Models decorate
// their answers — trailing prose, a second copy of the object — and the
// whole-string parse fails on any of it (baike-058: trailing garbage after
// a complete object cost the item twice). Returns "" when no balanced
// object exists.
func firstJSONObject(s string) string {
	start, depth := -1, 0
	inStr, esc := false, false
	for i, r := range s {
		switch {
		case esc:
			esc = false
		case inStr:
			if r == '"' {
				inStr = false
			} else if r == '\\' {
				esc = true
			}
		case r == '"':
			inStr = true
		case r == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case r == '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					return s[start : i+1]
				}
			}
		}
	}
	return ""
}

// ParseAnalyzeJSON is exported for frozen prompt regression tests.
func ParseAnalyzeJSON(raw string) (fast.Analysis, error) {
	if obj := firstJSONObject(raw); obj != "" {
		raw = obj
	}
	var parsed AnalyzeResult
	if err := parseJSON(raw, &parsed); err != nil {
		return fast.Analysis{}, err
	}
	if parsed.Intent == "" {
		parsed.Intent = fast.IntentSearch
	}
	// keywords_alt (synonyms) joins the secondary cascade alongside fallback.
	if len(parsed.Keywords) > 0 {
		if parsed.Fallback == nil {
			parsed.Fallback = map[string]float64{}
		}
		for k, v := range parsed.Keywords {
			if _, ok := parsed.Fallback[k]; !ok {
				parsed.Fallback[k] = v
			}
		}
	}
	return fast.Analysis{Intent: parsed.Intent, Primary: parsed.Primary, Fallback: parsed.Fallback}, nil
}

// AigateSynthesizer renders the answer summary via the synthesize_roi prompt
// (citation-marked, refuse-capable).
type AigateSynthesizer struct {
	Client *ChatClient

	mu      sync.Mutex
	refused bool
}

// Refused reports whether the last Synthesize was a refusal
// (synthesize_roi rejected the evidence). fast.RefusalReporter.
func (s *AigateSynthesizer) Refused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// Synthesize implements fast.Synthesizer.
func (s *AigateSynthesizer) Synthesize(ctx context.Context, query string, samples []mcs.Sample) (string, error) {
	s.setRefused(false)
	var ev strings.Builder
	for i, sm := range samples {
		fmt.Fprintf(&ev, "[%d] (%s [%d,%d)) %s\n", i+1, sm.Source, sm.Start, sm.End, truncateRunes(sm.Content, maxSampleRunes))
	}
	tmpl := prompts.MustRender(prompts.SynthesizeROI, map[string]string{
		"query":     query,
		"evidences": ev.String(),
	})
	// Synthesis runs thinking-disabled FIRST. Measured on MiniMax-M3 with
	// the real prompt and a 15K-rune legal evidence: 30.0s with thinking vs
	// 9.7s without, and the briefing structure, 条款号 tables and citations
	// all held (7 citations vs 5) — the chain-of-thought spend was not
	// buying answer quality on this workload. The thinking pass is kept as
	// the error-path retry: a failed or unparseable first response gets the
	// slower, more deliberate call instead of losing the query.
	raw, err := s.Client.CompleteStructured(ctx, tmpl)
	if err == nil {
		var out SynthesizeResult
		out, err = ParseSynthesizeJSON(raw)
		if err == nil && strings.TrimSpace(out.Summary) != "" {
			s.setRefused(out.Refuse)
			return out.Summary, nil
		}
	}
	// Retry with the thinking pass. A transport failure on the retry keeps
	// the FIRST error when there was one (the more informative one — a
	// status code beats re-parsing an empty body); a parse/summary failure
	// on the first pass judges the retry's response below.
	raw2, err2 := s.Client.Complete(ctx, tmpl)
	if err2 != nil {
		if err != nil {
			return "", err
		}
		return "", err2
	}
	out, perr := ParseSynthesizeJSON(raw2)
	if perr != nil {
		return "", perr
	}
	if strings.TrimSpace(out.Summary) == "" {
		return "", fmt.Errorf("llm: empty summary")
	}
	s.setRefused(out.Refuse)
	return out.Summary, nil
}

func (s *AigateSynthesizer) setRefused(v bool) {
	s.mu.Lock()
	s.refused = v
	s.mu.Unlock()
}

// SynthesizeResult is the synthesize_roi JSON shape.
type SynthesizeResult struct {
	Summary   string `json:"summary"`
	Citations []struct {
		Index int    `json:"index"`
		Quote string `json:"quote"`
	} `json:"citations"`
	ConfidenceNote string `json:"confidence_note"`
	Refuse         bool   `json:"refuse"`
}

// ParseSynthesizeJSON is exported for frozen prompt regression tests.
func ParseSynthesizeJSON(raw string) (SynthesizeResult, error) {
	var parsed SynthesizeResult
	if err := parseJSON(raw, &parsed); err != nil {
		return SynthesizeResult{}, err
	}
	return parsed, nil
}

// AigateKeywordExpander yields N keyword levels via the keywords_multilevel
// prompt — the deeper cascade when primary/fallback keywords miss.
type AigateKeywordExpander struct {
	Client *ChatClient
	Levels int
} // Expand implements fast.KeywordExpander.
// Thinking-disabled like Analyze: cascade-level keywords are a mechanical
// extraction pass and its reasoning is discarded. Only reached when the
// primary/fallback cascade already missed, so the saving is rare but free.
func (e *AigateKeywordExpander) Expand(ctx context.Context, query string, levels int) ([][]string, error) {
	if levels <= 0 {
		levels = e.Levels
	}
	if levels <= 0 {
		levels = 3
	}
	tmpl := prompts.MustRender(prompts.KeywordsMultilevel, map[string]string{
		"query":  query,
		"levels": strconv.Itoa(levels),
	})
	raw, err := e.Client.CompleteStructured(ctx, tmpl)
	if err != nil {
		return nil, err
	}
	return ParseMultilevelJSON(raw, levels)
}

// AigateKeywordRefiner regenerates keywords AFTER a failed match (ReAct
// 精炼轮 — the widen loop's second attempt when the whole cascade came up
// empty). Domain specialization is OPERATOR-declared via CLUS_DOMAIN_HINT and
// injected as a hint only; the prompt asset itself stays corpus-agnostic
// (评估纪律：管线资产不得携带评测语料的领域知识).
type AigateKeywordRefiner struct {
	Client *ChatClient
}

// Refine returns replacement keywords in the target corpus's register,
// excluding the failed ones.
func (r *AigateKeywordRefiner) Refine(ctx context.Context, query string, failed []string) ([]string, error) {
	domain := strings.TrimSpace(os.Getenv("CLUS_DOMAIN_HINT"))
	if domain == "" {
		domain = "通用文档（未指定领域）"
	}
	tmpl := prompts.MustRender(prompts.KeywordsRefine, map[string]string{
		"query":  query,
		"failed": strings.Join(failed, "、"),
		"domain": domain,
	})
	raw, err := r.Client.Complete(ctx, tmpl)
	if err != nil {
		return nil, err
	}
	clean, _ := SplitThink(raw)
	var parsed struct {
		Refined []string `json:"refined"`
	}
	if err := parseJSON(clean, &parsed); err != nil {
		return nil, err
	}
	out := parsed.Refined[:0]
	for _, k := range parsed.Refined {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out, nil
}

// ParseMultilevelJSON is exported for frozen prompt regression tests.
func ParseMultilevelJSON(raw string, levels int) ([][]string, error) {
	var parsed map[string][]string
	if err := parseJSON(raw, &parsed); err != nil {
		return nil, err
	}
	out := make([][]string, 0, levels)
	for i := 1; i <= levels; i++ {
		out = append(out, parsed[fmt.Sprintf("level_%d", i)])
	}
	return out, nil
}

// AigateQuerySimulator implements Self-Index A.2.1 two-call isolation:
// call 1 abstracts the information need from the raw query; call 2 writes
// complementary queries from the abstract only (never sees the original
// wording — prevents copy-the-source leakage). Results pass Jaccard
// dissimilarity against the origin and already-tried queries.
type AigateQuerySimulator struct {
	Client *ChatClient
	// Tau is the Jaccard keep threshold (candidates with Jac < tau survive).
	// 0 → facts.FilterDissimilar default 0.5.
	Tau float64
}

// Complement returns 0–N rephrasings distinct from origin+tried.
func (s *AigateQuerySimulator) Complement(ctx context.Context, origin string, tried []string) ([]string, error) {
	if s == nil || s.Client == nil || strings.TrimSpace(origin) == "" {
		return nil, nil
	}
	raw1, err := s.Client.Complete(ctx, prompts.MustRender(prompts.QueryAbstract, map[string]string{
		"query": origin,
	}))
	if err != nil {
		return nil, err
	}
	need, err := ParseQueryAbstractJSON(raw1)
	if err != nil || strings.TrimSpace(need) == "" {
		return nil, err
	}
	domain := strings.TrimSpace(os.Getenv("CLUS_DOMAIN_HINT"))
	if domain == "" {
		domain = "通用文档（未指定领域）"
	}
	seen := append([]string{}, tried...)
	raw2, err := s.Client.Complete(ctx, prompts.MustRender(prompts.QueryFromAbstract, map[string]string{
		"need":   need,
		"tried":  strings.Join(seen, "、"),
		"domain": domain,
	}))
	if err != nil {
		return nil, err
	}
	cands, err := ParseQueryListJSON(raw2)
	if err != nil {
		return nil, err
	}
	return facts.FilterDissimilar(origin, tried, cands, s.Tau), nil
}

// ParseQueryAbstractJSON is exported for frozen prompt regression tests.
func ParseQueryAbstractJSON(raw string) (string, error) {
	var parsed struct {
		Need string `json:"need"`
	}
	if err := parseJSON(raw, &parsed); err != nil {
		return "", err
	}
	return strings.TrimSpace(parsed.Need), nil
}

// ParseQueryListJSON is exported for frozen prompt regression tests.
func ParseQueryListJSON(raw string) ([]string, error) {
	var parsed struct {
		Queries []string `json:"queries"`
	}
	if err := parseJSON(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed.Queries, nil
}

// AigateHistoryRewriter folds dialogue/cluster history into a standalone
// query via the history_rewrite prompt; failures degrade to the raw query.
type AigateHistoryRewriter struct {
	Client *ChatClient
}

// Rewrite implements deep.HistoryRewriter.
func (r *AigateHistoryRewriter) Rewrite(ctx context.Context, history []string, query string) (string, error) {
	tmpl := prompts.MustRender(prompts.HistoryRewrite, map[string]string{
		"history": strings.Join(history, "\n"),
		"query":   query,
	})
	raw, err := r.Client.Complete(ctx, tmpl)
	if err != nil {
		return query, err
	}
	out, err := ParseHistoryRewriteJSON(raw)
	if err != nil {
		return query, err
	}
	if !out.HistoryRelevant || strings.TrimSpace(out.StandaloneQuery) == "" {
		return query, nil
	}
	return out.StandaloneQuery, nil
}

// HistoryRewriteResult is the history_rewrite JSON shape.
type HistoryRewriteResult struct {
	HistoryRelevant bool   `json:"history_relevant"`
	StandaloneQuery string `json:"standalone_query"`
	Changed         bool   `json:"changed"`
}

// ParseHistoryRewriteJSON is exported for frozen prompt regression tests.
func ParseHistoryRewriteJSON(raw string) (HistoryRewriteResult, error) {
	var parsed HistoryRewriteResult
	if err := parseJSON(raw, &parsed); err != nil {
		return HistoryRewriteResult{}, err
	}
	return parsed, nil
}

var (
	_ fast.Analyzer        = (*AigateAnalyzer)(nil)
	_ fast.Synthesizer     = (*AigateSynthesizer)(nil)
	_ fast.KeywordExpander = (*AigateKeywordExpander)(nil)
)

var jsonRe = regexp.MustCompile(`\{[\s\S]*\}`)

func parseJSON(raw string, v any) error {
	m := jsonRe.FindString(raw)
	if m == "" {
		return fmt.Errorf("llm: no JSON in response: %s", truncate(raw, 120))
	}
	err := json.Unmarshal([]byte(m), v)
	if err == nil {
		return nil
	}
	// LLM repair pass. Reasoning models emit two recurring malformed shapes
	// when they quote legal text: ASCII double quotes inside JSON string
	// values (教师应当"关心、爱护全体学生…" — invalid JSON) and raw newlines
	// inside those strings (also invalid). Either used to cost the whole
	// answer: the synthesizer fell back to its deterministic template and
	// the user saw a refusal for a question the corpus answers (live probe:
	// scorer 9/10 citing 第三十七条, synth failing with "invalid character
	// '关' after object key:value pair" on exactly that quote). Repair and
	// retry once; structurally sound responses take the strict path above
	// and never see this.
	if fixed := repairModelJSON(m); fixed != m {
		if err2 := json.Unmarshal([]byte(fixed), v); err2 == nil {
			return nil
		}
	}
	return err
}

// repairModelJSON makes an LLM's malformed JSON parseable without touching
// structure: inside a string, raw control characters become their escapes,
// and an ASCII double quote that cannot be the string's closing quote (the
// next non-space rune is not JSON punctuation , } ] :) is demoted to the
// matching full-width quote. The closing-quote test has to know the ":"
// case: it is what closes an object KEY ("summary": …), and mistaking that
// for content would corrupt every key. Content quotes alternate open/close
// by parity within the string (legal quotes come in pairs). A backslash
// escape is copied through verbatim and does not toggle state. Already
// well-formed input comes back byte-identical.
func repairModelJSON(s string) string {
	r := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	changed := false
	contentQuotes := 0
	for i := 0; i < len(r); i++ {
		c := r[i]
		if inString && c == '\\' && i+1 < len(r) {
			b.WriteRune(c)
			b.WriteRune(r[i+1])
			i++
			continue
		}
		if c == '"' && (!inString || closesString(r, i)) {
			inString = !inString
			contentQuotes = 0
			b.WriteRune(c)
			continue
		}
		if inString {
			switch c {
			case '\n':
				b.WriteString(`\n`)
				changed = true
				continue
			case '\r':
				b.WriteString(`\r`)
				changed = true
				continue
			case '\t':
				b.WriteString(`\t`)
				changed = true
				continue
			case '"':
				if contentQuotes%2 == 0 {
					b.WriteRune('“')
				} else {
					b.WriteRune('”')
				}
				contentQuotes++
				changed = true
				continue
			}
		}
		b.WriteRune(c)
	}
	if !changed {
		return s
	}
	return b.String()
}

// closesString reports whether the quote at r[i] is the string's real
// closing quote: the next non-space rune is JSON structure (, } ] :), or
// the text ends. A colon is included because it closes object keys.
func closesString(r []rune, i int) bool {
	next := rune(0)
	for j := i + 1; j < len(r); j++ {
		if !unicode.IsSpace(r[j]) {
			next = r[j]
			break
		}
	}
	switch next {
	case 0, ',', '}', ']', ':':
		return true
	}
	return false
}

// maxSampleRunes bounds how much of ONE evidence sample reaches the model.
// Both LLM consumers used to cap by BYTES (scorer 2000 ≈ 670 CJK runes,
// synthesizer 800 ≈ 270), calibrated for the ~240-rune hit windows the
// Monte-Carlo sampler emits. That cap silently decapitated whole-body
// evidence once the small-file full-body path landed: the sample spans the
// document, but the model only ever saw byte-800 of it — the file's opening
// articles — and then honestly refused ("evidence covers 第一条至第三条
// only") while the answer sat at rune 2,000. The reference implementation
// feeds its synthesizer the whole file up to _FAST_MAX_EVIDENCE_CHARS
// (sirchmunk search.py:1540, 15K chars) and answers from it; matching that,
// the cap is 15K RUNES — the sampler's own MaxEvidence budget — so a
// budgeted sample reaches the model in full, with the cap still netting
// callers that bypass the sampler.
const maxSampleRunes = 15_000

// truncateRunes caps s at n runes (not bytes): CJK text is ~3 bytes per
// rune, and byte caps stated in decimal digits under-serve it 3x — the
// whole failure mode this replaces.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// jsonFieldScrubber streams a JSON string field's body out of a model's
// token stream. It waits for "field" : " and then emits the unescaped body
// until the closing unescaped quote, ignoring everything before and after
// (the ```json fence, the envelope). Unparseable shapes fall out naturally:
// no field → nothing emitted; early close → partial body — both leave the
// caller to the non-streaming path, which replaces the partial text.
type jsonFieldScrubber struct {
	state   int // 0 seek key, 1 want colon, 2 want open quote, 3 body, 4 done
	needle  string
	matched int
	esc     rune // pending escape: '\\' backslash, 'u' unicode collection
	uni     []rune
}

func newJSONFieldScrubber(field string) *jsonFieldScrubber {
	return &jsonFieldScrubber{needle: `"` + field + `"`}
}

// scrubFeed consumes one delta and returns the text to forward (possibly
// empty); sc carries the state machine across deltas.
func scrubFeed(sc *jsonFieldScrubber, delta string) []string {
	if sc.state == 4 {
		return nil
	}
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range delta {
		switch sc.state {
		case 0: // seeking the field-name needle
			if r == rune(sc.needle[sc.matched]) {
				sc.matched++
				if sc.matched == len(sc.needle) {
					sc.state = 1
					sc.matched = 0
				}
				continue
			}
			sc.matched = 0
			if r == rune(sc.needle[0]) {
				sc.matched = 1
			}
		case 1: // after the key: want ':'
			if r == ':' {
				sc.state = 2
			}
		case 2: // after the colon: want the opening quote
			switch r {
			case '"':
				sc.state = 3
			case ' ', '\t', '\n', '\r':
			default:
				sc.state = 4 // unexpected shape: stop forwarding
			}
		case 3: // body
			if sc.esc != 0 {
				if got := sc.unescape(r); got != 0 {
					b.WriteRune(got)
					flush()
				}
				continue
			}
			switch r {
			case '\\':
				sc.esc = '\\'
			case '"':
				sc.state = 4 // closing quote: the envelope follows
			default:
				b.WriteRune(r)
				flush()
			}
		}
	}
	flush()
	return out
}

// unescape resolves one escape payload rune; \uXXXX accumulates here.
func (sc *jsonFieldScrubber) unescape(r rune) rune {
	if sc.esc == 'u' {
		sc.uni = append(sc.uni, r)
		if len(sc.uni) == 4 {
			v, err := strconv.ParseInt(string(sc.uni), 16, 32)
			sc.uni = sc.uni[:0]
			sc.esc = 0
			if err == nil {
				return rune(v)
			}
			return 0
		}
		return 0 // still collecting hex digits
	}
	sc.esc = 0
	switch r {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'r':
		return '\r'
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case '"':
		return '"'
	case '\\':
		return '\\'
	case '/':
		return '/'
	case 'u':
		sc.esc = 'u'
		sc.uni = sc.uni[:0]
		return 0
	}
	return r
}

// JudgeAnswer asks the no-reference persist judge: does this candidate
// answer actually answer the question — versus refusing, drifting off, or
// restating the question? Returns (ok, why, err). The caller decides
// record-vs-gate; an error here is NOT a verdict (the write path fails
// open on errors, closes on verdicts).
func (c *ChatClient) JudgeAnswer(ctx context.Context, query, answer string) (bool, string, error) {
	if c == nil {
		return false, "", errors.New("judge: no chat client")
	}
	tmpl := prompts.MustRender(prompts.JudgeAnswer, map[string]string{
		"query": query, "answer": answer,
	})
	raw, err := c.Complete(ctx, tmpl)
	if err != nil {
		return false, "", err
	}
	clean, _ := SplitThink(raw)
	var parsed struct {
		OK  bool   `json:"ok"`
		Why string `json:"why"`
	}
	if err := parseJSON(clean, &parsed); err != nil {
		return false, "", fmt.Errorf("judge: %w", err)
	}
	return parsed.OK, strings.TrimSpace(parsed.Why), nil
}
