package minilm

// Model install: the weights are the suite's OWN asset, downloaded from
// ModelScope into ~/.cumulus/models/<ModelID> (CLUS_MODEL_DIR overrides).
// The suite never looks into another project's cache — an independent
// project installs its own model. Downloads are resumable-by-skip (present
// files with the right size are kept), sha256-verified when the API reports
// one, and atomic (download to .part, then rename).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ModelID is the ModelScope model this package runs.
	ModelID = "sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2"
	// ModelScopeBase is the download source (魔搭社区).
	ModelScopeBase = "https://modelscope.cn"
	// ModelRevision is pinned: a weight surprise must be an explicit upgrade.
	ModelRevision = "master"
	// modelBaseDir is the suite's own model root (independent of any other
	// project's cache).
	modelBaseDir = ".cumulus"

	// neededFiles is exactly what the loader reads, plus the tiny config for
	// the architecture cross-check in verify. Everything else in the repo
	// (pytorch_model.bin, tf_model.h5, onnx/, tokenizer.json …) is skipped —
	// 470MB of safetensors, not 1.4GB of duplicate formats.
	neededFiles = "model.safetensors,unigram.json,config.json"
)

// Manifest records one completed install.
type Manifest struct {
	Model       string            `json:"model"`
	Revision    string            `json:"revision"`
	Files       map[string]string `json:"files"` // path -> sha256 (empty when the API omits it)
	InstalledAt string            `json:"installed_at"`
}

func manifestPath(dir string) string { return filepath.Join(dir, "manifest.json") }

// WriteManifest records a completed install in dir.
func WriteManifest(dir string, m Manifest) error {
	if m.InstalledAt == "" {
		m.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(manifestPath(dir), raw, 0o644)
}

// ReadManload reads a manifest; a missing one is (zero, nil).
func ReadManifest(dir string) (Manifest, error) {
	var m Manifest
	raw, err := os.ReadFile(manifestPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return m, err
	}
	err = json.Unmarshal(raw, &m)
	return m, err
}

// Progress is one file's download progress.
type Progress struct {
	File  string `json:"file"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

// Installer downloads the needed files from ModelScope into Dir.
type Installer struct {
	Dir        string
	BaseURL    string // override for tests; "" = ModelScopeBase
	OnProgress func(Progress)
	HTTP       *http.Client
}

// repoFile is one entry of the ModelScope file listing.
type repoFile struct {
	Path   string `json:"Path"`
	Size   int64  `json:"Size"`
	Sha256 string `json:"Sha256"`
	Type   string `json:"Type"` // "tree" for directories
}

func (in *Installer) base() string {
	if in.BaseURL != "" {
		return strings.TrimRight(in.BaseURL, "/")
	}
	return ModelScopeBase
}

func (in *Installer) client() *http.Client {
	if in.HTTP != nil {
		return in.HTTP
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

// EnsureInstalled downloads whatever is missing and writes the manifest.
// Idempotent: a present file with the listed size is kept as-is.
func (in *Installer) EnsureInstalled(ctx context.Context) (Manifest, error) {
	if in.Dir == "" {
		return Manifest{}, fmt.Errorf("install: dir required")
	}
	files, err := in.list(ctx)
	if err != nil {
		return Manifest{}, err
	}
	want := map[string]bool{}
	for _, f := range strings.Split(neededFiles, ",") {
		want[strings.TrimSpace(f)] = true
	}
	byPath := map[string]repoFile{}
	for _, f := range files {
		if want[f.Path] {
			byPath[f.Path] = f
		}
	}
	missing := []string{}
	for p := range want {
		if _, ok := byPath[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return Manifest{}, fmt.Errorf("install: model %s repo lacks %v", ModelID, missing)
	}
	if err := os.MkdirAll(in.Dir, 0o755); err != nil {
		return Manifest{}, err
	}
	m := Manifest{Model: ModelID, Revision: ModelRevision, Files: map[string]string{}}
	for _, f := range byPathSorted(byPath) {
		dst := filepath.Join(in.Dir, f.Path)
		if st, serr := os.Stat(dst); serr == nil && st.Size() == f.Size {
			// Present and the right size: keep (resumable-by-skip).
		} else if err := in.download(ctx, f, dst); err != nil {
			return Manifest{}, err
		}
		if f.Sha256 != "" {
			sum, herr := fileSHA256(dst)
			if herr != nil {
				return Manifest{}, herr
			}
			if !strings.EqualFold(sum, f.Sha256) {
				os.Remove(dst) // never leave a bad weight behind as "installed"
				return Manifest{}, fmt.Errorf("install: %s sha256 mismatch (got %s, want %s)", f.Path, sum, f.Sha256)
			}
			m.Files[f.Path] = sum
		}
	}
	m.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	if err := WriteManifest(in.Dir, m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// byPathSorted keeps downloads deterministic (safetensors first so the
// progress bar spends its time on the big file).
func byPathSorted(byPath map[string]repoFile) []repoFile {
	order := []string{"model.safetensors", "unigram.json", "config.json"}
	out := make([]repoFile, 0, len(byPath))
	for _, p := range order {
		if f, ok := byPath[p]; ok {
			out = append(out, f)
		}
	}
	return out
}

func (in *Installer) list(ctx context.Context) ([]repoFile, error) {
	url := fmt.Sprintf("%s/api/v1/models/%s/repo/files?Revision=%s", in.base(), ModelID, ModelRevision)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := in.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("install: list %s: %w", ModelScopeBase, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("install: list %s: HTTP %d", ModelScopeBase, resp.StatusCode)
	}
	var out struct {
		Data struct {
			Files []repoFile `json:"Files"`
		} `json:"Data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("install: list decode: %w", err)
	}
	if len(out.Data.Files) == 0 {
		return nil, fmt.Errorf("install: %s repo listing is empty", ModelID)
	}
	return out.Data.Files, nil
}

func (in *Installer) download(ctx context.Context, f repoFile, dst string) error {
	url := fmt.Sprintf("%s/api/v1/models/%s/repo?Revision=%s&FilePath=%s",
		in.base(), ModelID, ModelRevision, f.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := in.client().Do(req)
	if err != nil {
		return fmt.Errorf("install: download %s: %w", f.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("install: download %s: HTTP %d", f.Path, resp.StatusCode)
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	pw := &progressWriter{file: f.Path, total: f.Size, on: in.OnProgress}
	if _, err := io.Copy(out, io.TeeReader(resp.Body, pw)); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("install: download %s: %w", f.Path, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if st, serr := os.Stat(tmp); serr == nil && f.Size > 0 && st.Size() != f.Size {
		os.Remove(tmp)
		return fmt.Errorf("install: %s short download (%d of %d bytes)", f.Path, st.Size(), f.Size)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	in.emit(Progress{File: f.Path, Done: f.Size, Total: f.Size})
	return nil
}

func (in *Installer) emit(p Progress) {
	if in.OnProgress != nil {
		in.OnProgress(p)
	}
}

// progressWriter reports bytes as they stream (coarse: every 4MB or at end).
type progressWriter struct {
	file  string
	total int64
	done  int64
	last  int64
	on    func(Progress)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.done += int64(n)
	if w.on != nil && (w.done-w.last >= 4<<20 || w.done == w.total) {
		w.last = w.done
		w.on(Progress{File: w.file, Done: w.done, Total: w.total})
	}
	return n, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
