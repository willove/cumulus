package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubRoundTripper records request bodies and replays canned answers.
type stubRoundTripper struct {
	bodies  []string
	replies []string
	i       int
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	s.bodies = append(s.bodies, string(b))
	reply := s.replies[s.i]
	s.i++
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(reply)),
		Header:     make(http.Header),
	}, nil
}

// envelope wraps a raw assistant payload in the chat-completions shape the
// client parses.
func envelope(content string) string {
	return `{"choices":[{"message":{"content":` + jsonQuote(content) + `}}]}`
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func newStubClient(replies ...string) (*ChatClient, *stubRoundTripper) {
	wrapped := make([]string, len(replies))
	for i, r := range replies {
		wrapped[i] = envelope(r)
	}
	rt := &stubRoundTripper{replies: wrapped}
	return &ChatClient{BaseURL: "http://stub", Model: "m", HTTPClient: &http.Client{Transport: rt}}, rt
}

// 2.4 A.2.1 isolation: the two-call simulator must make the second call
// WITHOUT the raw original wording — otherwise it just paraphrases the
// query back and the complement adds nothing.
func TestQuerySimulatorSecondCallIsolatedFromOriginal(t *testing.T) {
	orig := "网购的东西七天无理由退货怎么退"
	chat, rt := newStubClient(
		`{"need": "消费者退货期限与无需说明理由的法定权利"}`,
		`{"queries": ["退货的合理期限在消保法里怎么规定", "无需说明理由的退货权依据是什么", "七天无理由退货怎么退"]}`,
	)
	sim := &QuerySimulator{Client: chat, Tau: 0.5}

	got, err := sim.Complement(context.Background(), orig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.bodies) != 2 {
		t.Fatalf("must be exactly two calls, got %d", len(rt.bodies))
	}
	if !strings.Contains(rt.bodies[0], orig) {
		t.Fatal("first call must receive the raw query (it abstracts it)")
	}
	if strings.Contains(rt.bodies[1], orig) {
		t.Fatal("second call must NOT receive the raw query (A.2.1 isolation)")
	}
	if !strings.Contains(rt.bodies[1], "消费者退货期限") {
		t.Fatal("second call must receive the abstraction")
	}
	// The near-copy of the original ("七天无理由退货怎么退" ≈ orig) must be
	// filtered out; the two complementary phrasings must survive.
	if len(got) != 2 {
		t.Fatalf("Jaccard filter must drop the near-copy, got %v", got)
	}
	for _, q := range got {
		if strings.Contains(q, "七天无理由") {
			t.Fatalf("near-copy survived the filter: %v", got)
		}
	}
}

// tried queries must be filtered (no repeats across rounds). Note the
// unigram-Jaccard filter is COARSE for short Chinese: "怎样才算商标性使用"
// shares 8/12 unigrams with the original and is dropped as a near-copy —
// τ is a reword-boundary knob, not a semantic judge.
func TestQuerySimulatorFiltersTriedQueries(t *testing.T) {
	orig := "怎样才算真的在使用商标"
	chat, _ := newStubClient(
		`{"need": "商标使用的法律定义与构成要件"}`,
		`{"queries": ["商标使用的定义是什么", "怎样才算商标性使用", "商标性使用的构成要件有哪些"]}`,
	)
	sim := &QuerySimulator{Client: chat, Tau: 0.5}
	tried := []string{"商标使用的定义是什么"}
	got, err := sim.Complement(context.Background(), orig, tried)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range got {
		if q == tried[0] {
			t.Fatalf("tried query must be dropped, got %v", got)
		}
	}
	// The genuinely different phrasing survives; the near-copy does not.
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "构成要件") {
		t.Fatalf("dissimilar complement must survive: %v", got)
	}
	if strings.Contains(joined, "怎样才算商标性使用") {
		t.Fatalf("near-copy must be dropped by Jaccard: %v", got)
	}
}

// A failed complement must not abort self-correction: the caller only uses
// the result when err == nil, so either (nil, err) or (nil, nil) is safe;
// what must never happen is a partial/garbage candidate list.
func TestQuerySimulatorDegradesQuietly(t *testing.T) {
	chat, _ := newStubClient(`not json at all`)
	sim := &QuerySimulator{Client: chat}
	got, err := sim.Complement(context.Background(), "任意问题", nil)
	if len(got) != 0 {
		t.Fatalf("bad abstraction must yield no complements, got %v", got)
	}
	// deep's self-correction does: `if extra, err := sim.Complement(...); err == nil && len(extra) > 0`
	// — an error is simply ignored, so returning one here is safe.
	_ = err
}
