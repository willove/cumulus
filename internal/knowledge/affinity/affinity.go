// Package affinity keeps a per-namespace token×document usage ledger.
//
// （移植自 cumulus 原版 internal/affinity，2026-10 重写时丢掉了，这里按原样带回。
//
//	端口层换成 cumulus-next 的 internal/store.Port，其余语义逐行保留——因为那些注释
//	里记的都是**踩过的坑**，重新推导一遍只会再踩一次。）
//
// Why it exists: the old history arm (prior.SigHistory) was global and
// binary — any document that ever served evidence scored a flat 1.0 for
// every later query until it aged out of a 200-window sample. Two adjacent
// questions in one domain ("在家养宠物影响到别人" → "宠物伤人") therefore
// shared nothing: the second re-ranked the whole corpus from scratch.
//
// The ledger records, per token, which documents repeatedly served queries
// CONTAINING that token. A read is query-conditioned: only the tokens of
// the incoming query contribute, each normalized by its own top document.
// CJK bigrams do the connecting (retrieval.Fields), so paraphrase/facet
// changes ("宠物扰邻" vs "宠物伤人") stay linked through shared tokens like
// "宠物" without any topic clustering having to be right.
//
// 2026-10 的实测（真实语料）：把后验做成**全局文档声望**是 −11pp（51 丢 / 18 赚）
// ——一篇文章回答很多问题、竞争文档各不相同，全局声望会把常被错引的热门文档沉底。
// 这就是**为什么账本默认只记不重排**（见 api 层的 ledgerRankEnabled）：一个只能从真实
// 问答里长出来的信号，不该用启发式参数硬开。
package affinity

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/store"
)

// LedgerEnabled 报告"账本是否参与重排"。**默认否**（只记不排）——理由见包注释：
// 账本的收益要用真实问答量出来，不能先信。原版把这个开关放在 api 层，这里保留同样的
// 默认值形状（false = 只记），由上层显式打开。
var LedgerEnabled = false

// EnableRerank 打开/关闭账本重排（供上层与测试显式设置）。
func EnableRerank(on bool) { LedgerEnabled = on }

// RerankEnabled 报告当前是否重排。
func RerankEnabled() bool { return LedgerEnabled }

// Defaults. All env-overridable at the serve face; the package itself stays
// deterministic for tests.
const (
	DefaultTauDays       = 21 // recency half-life-ish: weight halves every τ
	DefaultTopDocsPerTok = 60 // per-token doc cap (write-side prune)
	DefaultMinTokenLen   = 2  // single chars ("的") connect everything
	DefaultMaxTokensPerQ = 24 // query tokens actually scored
)

// OutcomeWeight scales how much an answer teaches. A confident answer is
// strong evidence; a weak one still records topical adjacency (0.15), which
// is what makes a budget-exhausted DEEP run leave a trace instead of nothing.
func OutcomeWeight(conf float64) float64 {
	switch {
	case conf >= 0.7:
		return 1.0
	case conf >= 0.4:
		return 0.4
	default:
		return 0.15
	}
}

// DocWeight is one ledger cell: a document that served a token's queries.
type DocWeight struct {
	SourceID string  `json:"source_id"`
	Score    float64 `json:"score"`
	Hits     int64   `json:"hits"`
	LastMS   int64   `json:"last_ms"`
}

// TokenDoc is the stored shape: ONE document per token. Reads become N point
// lookups instead of N scans; writes serialize per token under the store mu.
type TokenDoc struct {
	ID    string      `json:"_id"`
	Token string      `json:"token"`
	Docs  []DocWeight `json:"docs"`
}

// Ledger persists the token×document usage table in a store collection
// (default: affinity).
type Ledger struct {
	c    store.Port
	coll string
}

// storeLocks serializes the ledger's read-modify-write cycles per collection
// ACROSS store instances. The serve face constructs a store per request, so a
// per-instance mutex protected nothing: two concurrent Records for one token
// loaded the same document and the second save erased the first's increments
// (and raced Insert into ErrDuplicate, losing the record entirely).
var storeLocks sync.Map // coll → *sync.Mutex

func lockFor(coll string) *sync.Mutex {
	mu, _ := storeLocks.LoadOrStore(coll, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func NewLedger(c store.Port, coll string) *Ledger {
	if coll == "" {
		coll = Collection
	}
	return &Ledger{c: c, coll: coll}
}

// Collection 是账本集合名（按 realm 隔离，见 api 层 CollectionFor）。
const Collection = "affinity"

func tokenDocID(token string) string { return "aff|" + token }

// Record folds one answer's usage into the ledger: every (token, doc) pair
// gains w. Weight is applied once per pair, not per hit.
func (s *Ledger) Record(ctx context.Context, tokens []string, docIDs []string, w float64, now time.Time) error {
	if s == nil || w <= 0 || len(tokens) == 0 || len(docIDs) == 0 {
		return nil
	}
	nowMS := now.UnixMilli()
	mu := lockFor(s.coll)
	mu.Lock()
	defer mu.Unlock()
	for _, tok := range tokens {
		if len([]rune(tok)) < DefaultMinTokenLen {
			continue
		}
		doc, err := s.load(ctx, tok)
		if err != nil {
			return err
		}
		if doc == nil {
			doc = &TokenDoc{ID: tokenDocID(tok), Token: tok}
		}
		for _, id := range docIDs {
			if id == "" {
				continue
			}
			upsert(doc, id, w, nowMS)
		}
		prune(doc, DefaultTopDocsPerTok)
		if err := s.save(ctx, doc); err != nil {
			return err
		}
	}
	return nil
}

func upsert(doc *TokenDoc, id string, w float64, nowMS int64) {
	for i := range doc.Docs {
		if doc.Docs[i].SourceID == id {
			doc.Docs[i].Score += w
			doc.Docs[i].Hits++
			doc.Docs[i].LastMS = nowMS
			return
		}
	}
	doc.Docs = append(doc.Docs, DocWeight{SourceID: id, Score: w, Hits: 1, LastMS: nowMS})
}

// prune keeps the top-N cells by score so a hot token's document cannot grow
// without bound (the ledger is a prior, not an archive).
func prune(doc *TokenDoc, topN int) {
	if topN <= 0 || len(doc.Docs) <= topN {
		return
	}
	sort.Slice(doc.Docs, func(i, j int) bool { return doc.Docs[i].Score > doc.Docs[j].Score })
	doc.Docs = doc.Docs[:topN]
}

// Weights returns the normalized per-document prior for a query's tokens:
//
//	score(d) = Σ_t∈q decay(aff(t,d)) / Σ_t∈q max_d' decay(aff(t,d'))
//
// Each token is normalized by its own strongest document, so a token served
// by one strong document and a token served by ten documents contribute
// comparably. Tokens with no ledger presence, or fully decayed presence,
// contribute nothing. The result is in [0, len(tokens)].
func (s *Ledger) Weights(ctx context.Context, tokens []string, now time.Time) (map[string]float64, error) {
	out := map[string]float64{}
	if s == nil || len(tokens) == 0 {
		return out, nil
	}
	seen := map[string]bool{}
	n := 0
	for _, tok := range tokens {
		if len([]rune(tok)) < DefaultMinTokenLen || seen[tok] {
			continue
		}
		seen[tok] = true
		n++
		if n > DefaultMaxTokensPerQ {
			break
		}
		doc, err := s.load(ctx, tok)
		if err != nil || doc == nil || len(doc.Docs) == 0 {
			continue
		}
		var best float64
		decayed := make([]DocWeight, 0, len(doc.Docs))
		for _, dw := range doc.Docs {
			v := decay(dw.Score, dw.LastMS, now)
			if v > best {
				best = v
			}
			decayed = append(decayed, DocWeight{SourceID: dw.SourceID, Score: v})
		}
		if best <= 0 {
			continue // fully decayed token: silent, not a zero-normalized crash
		}
		for _, dw := range decayed {
			if dw.Score <= 0 {
				continue
			}
			out[dw.SourceID] += dw.Score / best
		}
	}
	return out, nil
}

// decay halves the weight every tauDays since last use: a document cited
// heavily last month still counts, but fades against this week's usage.
func decay(score float64, lastMS int64, now time.Time) float64 {
	if score <= 0 {
		return 0
	}
	if lastMS <= 0 {
		return score
	}
	age := now.Sub(time.UnixMilli(lastMS))
	if age <= 0 {
		return score
	}
	return score * exp2(-age.Hours()/(24*tauDays))
}

// tauDays is read through this var so tests can pin a fixed half-life.
var tauDays = float64(DefaultTauDays)

// exp2 is 2^x = e^(x·ln2).
func exp2(x float64) float64 {
	const ln2 = 0.6931471805599453
	return math.Exp(x * ln2)
}

func (s *Ledger) load(ctx context.Context, token string) (*TokenDoc, error) {
	var doc TokenDoc
	if err := s.c.GetStruct(ctx, s.coll, tokenDocID(token), &doc); err != nil {
		if errors.Is(err, contract.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if doc.ID == "" {
		doc.ID = tokenDocID(token)
	}
	return &doc, nil
}

func (s *Ledger) save(ctx context.Context, doc *TokenDoc) error {
	// PutStruct 是 upsert 语义，正好覆盖原版的 "Insert-or-Replace" 两段逻辑
	// （cumulite 的 ReplaceDocument + ErrDuplicate 回退在这里都不需要）。
	return s.c.PutStruct(ctx, s.coll, doc.ID, doc)
}

// TrimTokens keeps only ledger-shaped tokens (len ≥ 2, deduped, capped).
func TrimTokens(tokens []string, cap int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(strings.ToLower(t))
		if len([]rune(t)) < DefaultMinTokenLen || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if cap > 0 && len(out) >= cap {
			break
		}
	}
	return out
}

// CollectionFor 返回某 realm 的账本集合名（与 corpus 面的隔离口径一致）：
// 账本是**按租户分开的**——A 租户的问答不该改写 B 租户的排序先验。
func CollectionFor(realm string) string {
	if realm == "" {
		return Collection
	}
	return Collection + "_" + realm
}
