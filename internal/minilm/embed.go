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

// Required reports whether the operator demanded a hard failure instead of
// the silent hash fallback (CLUS_MINILM_REQUIRE=1). CI precision gates set
// it: a reference test that silently skips — or a search that silently
// degrades to hash-64 — must not read as "semantic path green".
func Required() bool {
	v := strings.TrimSpace(os.Getenv("CLUS_MINILM_REQUIRE"))
	return v == "1" || strings.EqualFold(v, "true")
}

// Resolve returns the real embedder when the weights are present, an error
// when Required demands them and they are absent, and (nil, nil) otherwise
// — the caller then keeps its offline fallback, exactly as before.
func Resolve() (*Embedder, error) {
	if Available() {
		return New(DefaultDir()), nil
	}
	if Required() {
		return nil, fmt.Errorf("minilm: CLUS_MINILM_REQUIRE=1 but weights absent at %s", DefaultDir())
	}
	return nil, nil
}

// DefaultDir resolves the suite's OWN model directory: $CLUS_MODEL_DIR (or
// the legacy $CLUS_MINILM_DIR override), else $HOME/.cumulus/models/<ModelID>.
// The suite never looks inside another project's cache — an independent
// project installs its own weights (see install.go / `model install`).
func DefaultDir() string {
	if v := os.Getenv("CLUS_MODEL_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("CLUS_MINILM_DIR"); v != "" {
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
