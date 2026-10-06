package minilm

// Embedder adapts the pure-Go MiniLM encoder to the cumulus-cluster suite's
// cluster.Embedder interface (float64 vectors — the store's vector JSON shape).
// Load is lazy: New is cheap, the 449MB mmap happens on first Embed.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Required reports whether the operator set CUMULUS_MINILM_REQUIRE. It no longer
// changes any behavior — Resolve fails on absent weights unconditionally — and
// survives only because gate harnesses still set it and /v1/config reports it.
func Required() bool {
	v := strings.TrimSpace(os.Getenv("CUMULUS_MINILM_REQUIRE"))
	return v == "1" || strings.EqualFold(v, "true")
}

// Resolve returns the real embedder when the weights are present, and an error
// when they are absent. Failing is the default, not an opt-in: the caller got
// here because someone asked for CUMULUS_EMBED=minilm, and the silent hash-64
// backfill it used to fall through to produces meaning-free vectors — Rerank
// then uses them to scramble BM25 order at random (measured 2026-10-01: serve
// ran days in that state because CUMULUS_EMBED was simply unset, and 道交法 fell
// out of DEEP's early windows on 闯红灯). The opt-out is CUMULUS_EMBED=hash, which
// never reaches this function, so an accident can't pass for a choice.
func Resolve() (*Embedder, error) {
	if Available() {
		return New(DefaultDir()), nil
	}
	return nil, fmt.Errorf("minilm: weights absent at %s — run `cumulus model install`, or set CUMULUS_EMBED=hash to accept non-semantic vectors", DefaultDir())
}

// DefaultDir resolves the suite's OWN model directory: $CUMULUS_MODEL_DIR (or
// the legacy $CUMULUS_MINILM_DIR override), else $HOME/.cumulus/models/<ModelID>.
// The suite never looks inside another project's cache — an independent
// project installs its own weights (see install.go / `model install`).
func DefaultDir() string {
	if v := os.Getenv("CUMULUS_MODEL_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("CUMULUS_MINILM_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, modelBaseDir, "models", ModelID)
}

// Available reports whether the model files are present at DefaultDir.
func Available() bool {
	d := DefaultDir()
	return d != "" &&
		fileExists(filepath.Join(d, "model.safetensors")) &&
		fileExists(filepath.Join(d, "unigram.json"))
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Embedder implements cluster.Embedder.
type Embedder struct {
	dir string

	mu   sync.Mutex
	m    *Model
	err  error
	done bool
}

// New returns a lazily-loading embedder over dir.
func New(dir string) *Embedder { return &Embedder{dir: dir} }

func (e *Embedder) load() (*Model, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.done {
		e.m, e.err = Load(e.dir)
		e.done = true
	}
	return e.m, e.err
}

// Embed returns one 384-dim L2-normalized vector per text.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	m, err := e.load()
	if err != nil {
		return nil, err
	}
	vecs, err := m.Encode(ctx, texts)
	if err != nil {
		return nil, err
	}
	out := make([][]float64, len(vecs))
	for i, v := range vecs {
		f := make([]float64, len(v))
		for j, x := range v {
			f[j] = float64(x)
		}
		out[i] = f
	}
	return out, nil
}

// Dims implements cluster.Embedder.
func (e *Embedder) Dims() int { return EmbeddingDim }
