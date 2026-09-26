package minilm

// The forward pass is a BertModel encoder (config.json: hidden 384, 12 layers,
// 12 heads × 32, intermediate 1536, gelu, eps 1e-12, absolute positions from
// 0 — verified against the model's own position_ids buffer) followed by the
// 1_Pooling config: mean pooling, L2 normalize.
//
// Performance model: a naive per-position matvec re-streams every weight
// matrix once per token (~15GB of memory traffic for a 128-token document).
// matmulRowsT instead blocks over output rows so a weight block stays
// cache-resident across all positions, and runs blocks in parallel. The
// inner dot product uses four independent accumulators (ILP: the scalar
// loop is latency-bound on a single dependency chain). Reordering moves
// results by ~1e-6 relative; the reference tests lock cosine ≥ 0.999, not
// bitwise identity.

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"sync"
)

// embedWorkerCap bounds the block-level matmul parallelism per call
// (GOMAXPROCS when unset — the pre-knob behaviour). A backfill on a
// workstation wants a cap: the text-level semaphore is 4, so peak
// goroutines are 4×this, and the uncapped default puts ~40 goroutines on a
// 10-core host and pegs it. CLUS_EMBED_WORKERS=2 keeps the machine usable.
var embedWorkerCap = parseWorkerCap(os.Getenv("CLUS_EMBED_WORKERS"))

// parseWorkerCap reads the CLUS_EMBED_WORKERS value: a positive integer caps
// the block-level parallelism; anything else (unset, garbage, <1) means no
// cap — GOMAXPROCS, the pre-knob behaviour. It never fails the process over
// a bad knob; the host just runs hot.
func parseWorkerCap(v string) int {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

const (
	hidden  = 384
	layers  = 12
	heads   = 12
	headDim = 32 // 384/12 — MiniLM multilingual uses 12 heads of 32 dims
	inter   = 1536
	maxPos  = 512
	lnEps   = 1e-12
	qkvOut  = 3 * hidden
	rowBloc = 64 // output rows per parallel block (worst block ≈ 64×1536×4B)
	// EmbeddingDim is the model's output dimensionality.
	EmbeddingDim = 384
)

// NW is a weight(+bias) pair; W is [out,in] row-major (nn.Linear layout).
type NW struct {
	W []float32
	B []float32
}

// Model is a loaded, ready-to-run encoder. The safetensors file stays mmapped;
// the QKV projections are concatenated once at load (single fused matmul per
// layer); word-embedding rows page in on demand.
type Model struct {
	tok *Tokenizer

	wordEmb []float32
	posEmb  []float32
	typEmb  []float32
	embLN   NW

	qkvW  [layers][]float32 // [qkvOut, hidden]
	qkvB  [layers][]float32
	ao    [layers]NW
	aoLN  [layers]NW
	iW    [layers][]float32 // [inter, hidden]
	iB    [layers][]float32
	oW    [layers][]float32 // [hidden, inter]
	oB    [layers][]float32
	outLN [layers]NW
}

// Load opens the model dir (model.safetensors + unigram.json) and binds the
// weight references.
func Load(dir string) (*Model, error) {
	st, err := openSafetensors(dir + "/model.safetensors")
	if err != nil {
		return nil, err
	}
	tok, err := LoadTokenizer(dir)
	if err != nil {
		return nil, err
	}
	m := &Model{tok: tok}
	g := func(name string) []float32 {
		t, err := st.tensor(name)
		if err != nil {
			// The checkpoint layout is pinned by tests; a missing tensor is a
			// spec violation, not a runtime condition.
			panic(err)
		}
		return t
	}
	nw := func(base string) NW {
		return NW{W: g(base + ".weight"), B: g(base + ".bias")}
	}
	if pos, err := st.i64Row("embeddings.position_ids"); err == nil && len(pos) > 0 && pos[0] != 0 {
		return nil, fmt.Errorf("minilm: position_ids starts at %d — spec drift", pos[0])
	}
	m.wordEmb = g("embeddings.word_embeddings.weight")
	m.posEmb = g("embeddings.position_embeddings.weight")
	m.typEmb = g("embeddings.token_type_embeddings.weight")
	m.embLN = nw("embeddings.LayerNorm")
	for i := 0; i < layers; i++ {
		base := "encoder.layer." + strconv.Itoa(i) + "."
		q, k, v := nw(base+"attention.self.query"), nw(base+"attention.self.key"), nw(base+"attention.self.value")
		m.qkvW[i] = make([]float32, 0, qkvOut*hidden)
		m.qkvB[i] = make([]float32, 0, qkvOut)
		for _, p := range []NW{q, k, v} {
			m.qkvW[i] = append(m.qkvW[i], p.W...)
			m.qkvB[i] = append(m.qkvB[i], p.B...)
		}
		m.ao[i] = nw(base + "attention.output.dense")
		m.aoLN[i] = nw(base + "attention.output.LayerNorm")
		m.iW[i] = g(base + "intermediate.dense.weight")
		m.iB[i] = g(base + "intermediate.dense.bias")
		m.oW[i] = g(base + "output.dense.weight")
		m.oB[i] = g(base + "output.dense.bias")
		m.outLN[i] = nw(base + "output.LayerNorm")
	}
	return m, nil
}

// Tokenizer exposes the loaded tokenizer.
func (m *Model) Tokenizer() *Tokenizer { return m.tok }

// matmulRowsT computes y[p*out+o] = Σ_i x[p*in+i]·W[o*in+i] + b[o] for all
// positions p. Blocked over output rows (weight blocks stay cache-resident
// across positions), blocks fanned out across CPUs; disjoint y regions keep
// it race-free, and the accumulation order per element matches a plain loop.
func matmulRowsT(x, w, b []float32, L, out, in int, y []float32) {
	blocks := (out + rowBloc - 1) / rowBloc
	workers := runtime.GOMAXPROCS(0)
	if embedWorkerCap > 0 && embedWorkerCap < workers {
		workers = embedWorkerCap
	}
	if blocks < workers {
		workers = blocks
	}
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for bl := worker; bl < blocks; bl += workers {
				o0 := bl * rowBloc
				o1 := o0 + rowBloc
				if o1 > out {
					o1 = out
				}
				for p := 0; p < L; p++ {
					xp := x[p*in : p*in+in]
					yp := y[p*out : p*out+out]
					for o := o0; o < o1; o++ {
						row := w[o*in : o*in+in]
						// Four independent accumulators break the
						// add-dependency chain (the scalar loop is
						// latency-bound, not throughput-bound).
						var a0, a1, a2, a3 float32
						i := 0
						for ; i+4 <= in; i += 4 {
							a0 += xp[i] * row[i]
							a1 += xp[i+1] * row[i+1]
							a2 += xp[i+2] * row[i+2]
							a3 += xp[i+3] * row[i+3]
						}
						for ; i < in; i++ {
							a0 += xp[i] * row[i]
						}
						yp[o] = (a0 + a1) + (a2 + a3) + b[o]
					}
				}
			}
		}(worker)
	}
	wg.Wait()
}

// workspace is the per-encode scratch space (reused via sync.Pool; a full
// batch allocates once instead of ~2.5MB per sequence). Every buffer is
// fully overwritten before read except pool, which clears itself.
type workspace struct {
	h, qkv, ctxb, att, ff, sc, pool []float32
}

func newWorkspace(seq int) *workspace {
	return &workspace{
		h:    make([]float32, seq*hidden),
		qkv:  make([]float32, seq*qkvOut),
		ctxb: make([]float32, seq*hidden),
		att:  make([]float32, seq*hidden),
		ff:   make([]float32, seq*inter),
		sc:   make([]float32, heads*seq),
		pool: make([]float32, hidden),
	}
}

var wsPool = sync.Pool{New: func() any { return newWorkspace(maxSeqTokens) }}

// EncodeTokenVectors runs the encoder over token ids and returns the pooled,
// L2-normalized 384-dim vector.
func (m *Model) EncodeTokenVectors(ids []int) []float32 {
	L := len(ids)
	ws := wsPool.Get().(*workspace)
	if L > cap(ws.h)/hidden { // exported-path guard; tokenizer caps at 128
		ws = newWorkspace(L)
	}
	h := ws.h[:L*hidden]
	qkv := ws.qkv[:L*qkvOut]
	ctxb := ws.ctxb[:L*hidden]
	att := ws.att[:L*hidden]
	ff := ws.ff[:L*inter]
	sc := ws.sc[:heads*L]
	defer func() {
		if L <= cap(ws.h)/hidden {
			wsPool.Put(ws)
		}
	}()

	for p, id := range ids {
		copy(h[p*hidden:p*hidden+hidden], m.wordEmb[id*hidden:(id+1)*hidden])
		row := h[p*hidden : p*hidden+hidden]
		pos := p
		if pos >= maxPos {
			pos = maxPos - 1
		}
		for d := 0; d < hidden; d++ {
			row[d] += m.posEmb[pos*hidden+d] + m.typEmb[d]
		}
	}
	for p := 0; p < L; p++ {
		layerNorm(h[p*hidden:p*hidden+hidden], m.embLN.W, m.embLN.B, lnEps)
	}

	// Fused QKV buffer layout per position: [0:384]=Q, [384:768]=K, [768:1152]=V.
	for l := 0; l < layers; l++ {
		matmulRowsT(h, m.qkvW[l], m.qkvB[l], L, qkvOut, hidden, qkv)

		scale := float32(1 / math.Sqrt(headDim))
		for p := 0; p < L; p++ {
			for hh := 0; hh < heads; hh++ {
				o := hh * headDim
				qo := p*qkvOut + o
				kbase := hidden + o
				vbase := 2*hidden + o
				var maxv float32 = -1e30
				for kk := 0; kk < L; kk++ {
					qr := qkv[qo : qo+headDim]
					kr := qkv[kk*qkvOut+kbase : kk*qkvOut+kbase+headDim]
					var d float32
					for j := 0; j < headDim; j++ {
						d += qr[j] * kr[j]
					}
					s := d * scale
					sc[hh*L+kk] = s
					if s > maxv {
						maxv = s
					}
				}
				var sum float32
				for kk := 0; kk < L; kk++ {
					e := float32(math.Exp(float64(sc[hh*L+kk] - maxv)))
					sc[hh*L+kk] = e
					sum += e
				}
				inv := 1 / sum
				out := ctxb[p*hidden+o : p*hidden+o+headDim]
				clear(out)
				for kk := 0; kk < L; kk++ {
					wgt := sc[hh*L+kk] * inv
					vr := qkv[kk*qkvOut+vbase : kk*qkvOut+vbase+headDim]
					for d := 0; d < headDim; d++ {
						out[d] += wgt * vr[d]
					}
				}
			}
		}

		matmulRowsT(ctxb, m.ao[l].W, m.ao[l].B, L, hidden, hidden, att)
		for i := range h {
			h[i] += att[i]
		}
		for p := 0; p < L; p++ {
			layerNorm(h[p*hidden:p*hidden+hidden], m.aoLN[l].W, m.aoLN[l].B, lnEps)
		}

		matmulRowsT(h, m.iW[l], m.iB[l], L, inter, hidden, ff)
		for i := range ff {
			ff[i] = gelu(ff[i])
		}
		matmulRowsT(ff, m.oW[l], m.oB[l], L, hidden, inter, att)
		for i := range h {
			h[i] += att[i]
		}
		for p := 0; p < L; p++ {
			layerNorm(h[p*hidden:p*hidden+hidden], m.outLN[l].W, m.outLN[l].B, lnEps)
		}
	}

	// Mean pooling over all tokens (no padding in this path) + L2 normalize.
	pool := ws.pool[:]
	clear(pool)
	for p := 0; p < L; p++ {
		for d := 0; d < hidden; d++ {
			pool[d] += h[p*hidden+d]
		}
	}
	inv := 1 / float32(L)
	var norm float64
	for d := range pool {
		pool[d] *= inv
		norm += float64(pool[d]) * float64(pool[d])
	}
	invN := float32(1 / math.Sqrt(norm))
	for d := range pool {
		pool[d] *= invN
	}
	// pool aliases the pooled workspace: copy before returning, or the next
	// encode overwrites the caller's vector (batch results cross-contaminate).
	out := make([]float32, hidden)
	copy(out, pool)
	return out
}

// Encode embeds texts (batch); each result is a 384-dim L2-normalized vector.
// Texts run in parallel; ctx cancellation is checked between texts.
func (m *Model) Encode(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	sem := make(chan struct{}, 4)
	errs := make([]error, len(texts))
	var wg sync.WaitGroup
	for i, t := range texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = m.EncodeTokenVectors(m.tok.Encode(t))
		}(i, t)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	return out, nil
}
