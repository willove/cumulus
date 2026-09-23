package facts

import "strings"

// Jaccard is word-set Jaccard over Unigram-style tokens (Self-Index A.2.2
// Dissimilarity). Returns 0 when either side has no tokens (empty ∩ empty
// is treated as dissimilar so callers keep at least one candidate).
func Jaccard(a, b string) float64 {
	sa := tokenSet(a)
	sb := tokenSet(b)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	inter := 0
	for t := range sa {
		if sb[t] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// FilterDissimilar keeps candidates whose Jaccard with every keep-set member
// (and with `origin`) is < tau. tried is the already-used query list.
func FilterDissimilar(origin string, tried []string, cands []string, tau float64) []string {
	if tau <= 0 {
		tau = 0.5
	}
	var out []string
	seen := map[string]bool{}
	baseline := append([]string{origin}, tried...)
	for _, cand := range cands {
		cand = strings.TrimSpace(cand)
		if cand == "" || seen[cand] {
			continue
		}
		dup := false
		for _, b := range baseline {
			if b == cand || Jaccard(b, cand) >= tau {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		seen[cand] = true
		out = append(out, cand)
		baseline = append(baseline, cand)
	}
	return out
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.Fields(strings.ToLower(s)) {
		// CJK: also emit unigrams so 退货/退款 style partial overlap counts.
		var cur []rune
		flush := func() {
			if len(cur) > 0 {
				out[string(cur)] = true
				cur = cur[:0]
			}
		}
		for _, r := range tok {
			if r >= 0x4e00 && r <= 0x9fff {
				flush()
				out[string(r)] = true
				continue
			}
			cur = append(cur, r)
		}
		flush()
		if len(tok) > 0 {
			out[tok] = true
		}
	}
	return out
}
