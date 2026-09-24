package main

// The model face: first-run weight verification and guided install. The
// weights are the suite's OWN asset — ModelScope (魔搭社区) →
// ~/.cumulus/models/<ModelID> (CLUS_MODEL_DIR overrides) — never another
// project's cache. The CLI offers an interactive download; the workbench
// 配置 page does the same over REST (modelapi.go).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/willove/cumulus/internal/minilm"
)

type modelFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// modelStatus is the on-disk picture of the embedder seat.
type modelStatus struct {
	Installed bool            `json:"installed"`
	Dir       string          `json:"dir"`
	Dims      int             `json:"dims"`
	Files     []modelFile     `json:"files"`
	Manifest  minilm.Manifest `json:"manifest"`
}

// collectModelStatus reports what is on disk (no model load).
func collectModelStatus(dir string) modelStatus {
	st := modelStatus{Dir: dir, Dims: minilm.EmbeddingDim, Files: []modelFile{}}
	if m, err := minilm.ReadManifest(dir); err == nil {
		st.Manifest = m
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return st
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		st.Files = append(st.Files, modelFile{Name: e.Name(), Size: info.Size()})
	}
	st.Installed = minilm.Available() && fileExists(filepath.Join(dir, "model.safetensors"))
	return st
}

// installModel downloads whatever is missing, reporting progress.
func installModel(ctx context.Context, dir string, onProgress func(minilm.Progress)) (minilm.Manifest, error) {
	in := &minilm.Installer{Dir: dir, OnProgress: onProgress}
	return in.EnsureInstalled(ctx)
}

// verifyModel loads the weights and runs one probe encode — the install is
// only done when the forward pass actually runs.
func verifyModel(ctx context.Context, dir string) (map[string]any, error) {
	if !fileExists(filepath.Join(dir, "model.safetensors")) {
		return nil, fmt.Errorf("model: weights absent at %s — run `cumulus-cluster model install`", dir)
	}
	start := time.Now()
	var vecs [][]float32
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("model: weights failed to load (%v) — reinstall: cumulus-cluster model install", r)
			}
		}()
		m, lerr := minilm.Load(dir)
		if lerr != nil {
			return lerr
		}
		vecs, err = m.Encode(ctx, []string{"权重自检：连接池最大连接数是多少"})
		return err
	}()
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 || len(vecs[0]) != minilm.EmbeddingDim {
		return nil, fmt.Errorf("model: probe returned %d vectors (want 1×%d)", len(vecs), minilm.EmbeddingDim)
	}
	var norm float64
	for _, x := range vecs[0] {
		norm += float64(x) * float64(x)
	}
	return map[string]any{
		"ok": true, "dims": len(vecs[0]), "ms": time.Since(start).Milliseconds(),
		"norm": norm, "probe": vecs[0][:4],
	}, nil
}

// confirmDownload asks before a ~464MB download; a non-answer is a no.
func confirmDownload(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprintf(out, "%s [y/N] ", prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch line[:minInt(len(line), 1)] {
	case "y", "Y":
		return true
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// isInteractive reports whether stdin looks like a terminal (a prompt in a
// pipe would hang CI forever).
func isInteractive(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// maybeOfferModelInstall is the startup weight check: when the semantic
// embedder is requested but the weights are absent, say so and — on an
// interactive terminal — offer to download right now. Non-interactive runs
// get the reminder only (CI: `model install -y`).
func maybeOfferModelInstall(ctx context.Context) {
	if os.Getenv("CLUS_EMBED") != "minilm" || minilm.Available() {
		return
	}
	dir := minilm.DefaultDir()
	fmt.Fprintf(os.Stderr, "[model] MiniLM 权重缺席：%s\n", dir)
	fmt.Fprintln(os.Stderr, "[model] 安装：cumulus-cluster model install（约 485MB，魔搭社区）；或工作台「配置」页下载")
	if !isInteractive(os.Stdin) {
		return
	}
	if !confirmDownload(os.Stdin, os.Stderr, "[model] 是否现在下载并验证？") {
		return
	}
	if _, err := installModel(ctx, dir, func(p minilm.Progress) {
		fmt.Fprintf(os.Stderr, "\r[model] %s %d/%d MB", p.File, p.Done>>20, p.Total>>20)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "\n[model] 下载失败：%v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "\n[model] 下载完成，验证中……")
	rep, err := verifyModel(ctx, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[model] 验证失败：%v\n", err)
		return
	}
	b, _ := json.Marshal(rep)
	fmt.Fprintf(os.Stderr, "[model] 权重就绪：%s\n", b)
}
