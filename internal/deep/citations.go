// citations — 引用交付面：Ref 构建、未定位图例、文本工具。
// 从 deep.go 纯搬运（2026-10-04 拆分），无语义改动。
package deep

import (
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/source"
)

// Ref is one citation into a source (delivery face).
type Ref struct {
	Index    int    `json:"index"`
	SourceID string `json:"source_id"`
	Title    string `json:"title"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Quote    string `json:"quote"`
	Span     string `json:"span"` // structure label, e.g. p2 / §连接池
	Resolved bool   `json:"resolved"`
}

// CitationSet is the answer's evidence delivery: numbered refs + legend.
type CitationSet struct {
	Refs   []Ref  `json:"refs"`
	Legend string `json:"legend"`
}

// BuildCitations maps answer samples back to source spans, so a citation
// resolves to the original text.
// Each sample resolves against its own source: DEEP keeps multi-source windows
// with sm.Source = doc id, while FAST/cluster-reuse samples carry a sampling
// method label and fall back to the answer's single source. Resolved means the
// window pins back exactly — quote equals the current body slice — so stale
// windows (source updated after sampling) surface as [?] instead of现证.
func BuildCitations(query string, ans fast.Answer, sources []source.Source) CitationSet {
	var refs []Ref
	srcMap := map[string]source.Source{}
	for _, s := range sources {
		srcMap[s.ID] = s
	}
	i := 0
	for _, sm := range cluster.NormalizeEvidence(ans.SourceID, ans.Samples) {
		i++
		id := sm.Source
		src := srcMap[id]
		body := []rune(src.Body)
		r := Ref{
			Index: i, SourceID: id, Title: src.Title,
			Start: sm.Start, End: sm.End,
			Quote: trim(sm.Content, 120),
		}
		r.Resolved = sm.Start >= 0 && sm.Start < sm.End && sm.End <= len(body) &&
			string(body[sm.Start:sm.End]) == sm.Content
		if src.Body != "" {
			for _, sp := range src.Structure {
				if sp.Start <= sm.Start && sm.End <= sp.End {
					r.Span = sp.Label
					break
				}
			}
		}
		// [?] when the window cannot be pinned back.
		if !r.Resolved {
			r.Quote = "[?] " + r.Quote
		}
		refs = append(refs, r)
	}
	return CitationSet{Refs: refs}
}

func legend(cs CitationSet, unresolved bool) string {
	base := "引用编号 [n] 对应下方 refs；(span) 为原文定位标签。"
	if unresolved {
		return base + " [?] 表示引用未能精确回溯原文窗口（源已更新或定位越界），请以原文为准。"
	}
	return base
}

func min1(x float64) float64 {
	if x > 1 {
		return 1
	}
	return x
}

func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
