// Package llm adapts chat-completions endpoints (aigate / OpenAI-compatible)
// to cumulus-cluster's Scorer and Embedder interfaces. Offline KeywordScorer/Local stay
// the gate carrier; this package is the production path (S5 plan D6).
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
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
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", err
	}
	if out.Usage.TotalTokens > 0 {
		c.total.Add(out.Usage.TotalTokens) // budget accounting
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
	allowed := map[string]bool{}
	for _, f := range facts {
		if i := strings.Index(f, ":"); i > 0 {
			f = f[:i]
		}
		allowed[f] = true
	}
	var covers []string
	for _, c := range r.Covers {
		if allowed[c] {
			covers = append(covers, c)
		}
	}
	return r.Score, r.Reasoning, covers, nil
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
	raw, err := s.Client.Complete(ctx, tmpl)
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
		return fast.Analysis{}, err
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

// AnalyzeResult is the fast_analyze JSON shape.
type AnalyzeResult struct {
	Intent   string             `json:"intent"`
	Primary  map[string]float64 `json:"primary"`
	Fallback map[string]float64 `json:"fallback"`
	Keywords map[string]float64 `json:"keywords_alt"`
}

// ParseAnalyzeJSON is exported for frozen prompt regression tests.
func ParseAnalyzeJSON(raw string) (fast.Analysis, error) {
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
		domain = "未指定——按通用书面文档处理"
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
		domain = "未指定——按通用书面文档处理"
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
