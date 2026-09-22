package minilm

// Embedder adapts the pure-Go MiniLM encoder to the ask suite's
// cluster.Embedder interface (float64 vectors — the cumudb vector JSON shape).
// Load is lazy: New is cheap, the 449MB mmap happens on first Embed.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
)

// DefaultDir resolves the model directory: $ASK_MINILM_DIR, else Sirchmunk's
// on-disk cache (the operator already has it; no duplicate 449MB download).
func DefaultDir() string {
	if v := os.Getenv("ASK_MINILM_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".sirchmunk", ".cache", "models", "models",
		"sentence-transformers--paraphrase-multilingual-MiniLM-L12-v2", "snapshots", "master")
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
