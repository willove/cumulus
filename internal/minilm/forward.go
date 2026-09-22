package minilm

// The forward pass is a BertModel encoder (config.json: hidden 384, 12 layers,
// 12 heads × 64, intermediate 1536, gelu, eps 1e-12, absolute positions from
// 0 — verified against the model's own position_ids buffer) followed by the
// 1_Pooling config: mean pooling, L2 normalize.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
)

const (
	hidden  = 384
	layers  = 12
	heads   = 12
	headDim = 32 // 384/12 — MiniLM multilingual uses 12 heads of 32 dims
	inter   = 1536
	maxPos  = 512
	lnEps   = 1e-12
	// EmbeddingDim is the model's output dimensionality.
	EmbeddingDim = 384
)

// NW is a weight(+bias) pair; W is [out,in] row-major (nn.Linear layout).
type NW struct {
	W []float32
	B []float32
}

// qkv bundles one attention layer's three projections.
type qkv struct {
	Q, K, V NW
}

// Model is a loaded, ready-to-run encoder. The safetensors file stays mmapped;
// word-embedding rows page in on demand (~384MB of the weight blob is the
// embedding table and is touched sparsely).
type Model struct {
	tok *Tokenizer

	wordEmb []float32
	posEmb  []float32
	typEmb  []float32
	embLN   NW

	self  [layers]qkv
	ao    [layers]NW // attention output dense
	aoLN  [layers]NW
	iW    [layers][]float32 // intermediate weight [inter,hidden]
	iB    [layers][]float32
	oW    [layers][]float32 // output dense weight [hidden,inter]
	oB    [layers][]float32
	outLN [layers]NW
}

// Load opens the model dir (model.safetensors + unigram.json +
// normalization_map.json) and binds the weight references.
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
		m.self[i] = qkv{
			Q: nw(base + "attention.self.query"),
			K: nw(base + "attention.self.key"),
			V: nw(base + "attention.self.value"),
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

// EncodeTokenVectors runs the encoder over token ids and returns the pooled,
// L2-normalized 384-dim vector.
func (m *Model) EncodeTokenVectors(ids []int) []float32 {
	L := len(ids)
	h := make([]float32, L*hidden)
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

	q := make([]float32, L*hidden)
	k := make([]float32, L*hidden)
	v := make([]float32, L*hidden)
	ctxb := make([]float32, L*hidden)
	sc := make([]float32, heads*L)
	att := make([]float32, L*hidden)
	ff := make([]float32, inter)

	for l := 0; l < layers; l++ {
		for p := 0; p < L; p++ {
			x := h[p*hidden : p*hidden+hidden]
			matvecRowMajor(x, m.self[l].Q.W, m.self[l].Q.B, hidden, hidden, q[p*hidden:p*hidden+hidden])
			matvecRowMajor(x, m.self[l].K.W, m.self[l].K.B, hidden, hidden, k[p*hidden:p*hidden+hidden])
			matvecRowMajor(x, m.self[l].V.W, m.self[l].V.B, hidden, hidden, v[p*hidden:p*hidden+hidden])
		}
		scale := float32(1 / math.Sqrt(headDim))
		for p := 0; p < L; p++ {
			for hh := 0; hh < heads; hh++ {
				o := hh * headDim
				var maxv float32 = -1e30
				for kk := 0; kk < L; kk++ {
					qr := q[p*hidden+o : p*hidden+o+headDim]
					kr := k[kk*hidden+o : kk*hidden+o+headDim]
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
					w := sc[hh*L+kk] * inv
					vr := v[kk*hidden+o : kk*hidden+o+headDim]
					for d := 0; d < headDim; d++ {
						out[d] += w * vr[d]
					}
				}
			}
		}
		for p := 0; p < L; p++ {
			x := h[p*hidden : p*hidden+hidden]
			matvecRowMajor(ctxb[p*hidden:p*hidden+hidden], m.ao[l].W, m.ao[l].B, hidden, hidden, att[p*hidden:p*hidden+hidden])
			for d := 0; d < hidden; d++ {
				x[d] += att[p*hidden+d]
			}
			layerNorm(x, m.aoLN[l].W, m.aoLN[l].B, lnEps)
			matvecRowMajor(x, m.iW[l], m.iB[l], inter, hidden, ff)
			for d := 0; d < inter; d++ {
				ff[d] = gelu(ff[d])
			}
			matvecRowMajor(ff, m.oW[l], m.oB[l], hidden, inter, att[p*hidden:p*hidden+hidden])
			for d := 0; d < hidden; d++ {
				x[d] += att[p*hidden+d]
			}
			layerNorm(x, m.outLN[l].W, m.outLN[l].B, lnEps)
		}
	}

	// Mean pooling over all tokens (no padding in this path) + L2 normalize.
	pool := make([]float32, hidden)
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
	return pool
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
