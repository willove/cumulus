// Package llm adapts chat-completions endpoints (aigate / OpenAI-compatible)
// to cumulus-cluster's Scorer and Embedder interfaces. Offline KeywordScorer/Local stay
// the gate carrier; this package is the production path (S5 plan D6).
package llm

import (
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
		"sample_content": truncate(sm.Content, 2000),
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
func (a *AigateAnalyzer) Analyze(ctx context.Context, query string) (fast.Analysis, error) {
	tmpl := prompts.MustRender(prompts.FastAnalyze, map[string]string{"query": query})
	raw, err := a.Client.Complete(ctx, tmpl)
	if err != nil {
		return fast.Analysis{}, err
	}
	return ParseAnalyzeJSON(raw)
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
		fmt.Fprintf(&ev, "[%d] (%s [%d,%d)) %s\n", i+1, sm.Source, sm.Start, sm.End, truncate(sm.Content, 800))
	}
	tmpl := prompts.MustRender(prompts.SynthesizeROI, map[string]string{
		"query":     query,
		"evidences": ev.String(),
	})
	raw, err := s.Client.Complete(ctx, tmpl)
	if err != nil {
		return "", err
	}
	out, err := ParseSynthesizeJSON(raw)
	if err != nil {
		return "", err
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
	raw, err := e.Client.Complete(ctx, tmpl)
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
	return json.Unmarshal([]byte(m), v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
