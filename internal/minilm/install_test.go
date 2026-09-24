package minilm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeModelScope serves the two endpoints the installer uses: the file
// listing and per-file bytes.
func fakeModelScope(t *testing.T, files map[string]string, shas map[string]string) (*httptest.Server, *int) {
	t.Helper()
	downloads := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/models/"+ModelID+"/repo/files", func(w http.ResponseWriter, r *http.Request) {
		type f struct {
			Path   string `json:"Path"`
			Size   int64  `json:"Size"`
			Sha256 string `json:"Sha256"`
			Type   string `json:"Type"`
		}
		var list []f
		for p, body := range files {
			list = append(list, f{Path: p, Size: int64(len(body)), Sha256: shas[p]})
		}
		list = append(list, f{Path: "pytorch_model.bin", Size: 999, Type: "0"}) // decoy: not needed
		fmt.Fprintf(w, `{"Data":{"Files":[`)
		for i, e := range list {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"Path":%q,"Size":%d,"Sha256":%q,"Type":%q}`, e.Path, e.Size, e.Sha256, e.Type)
		}
		fmt.Fprint(w, `]}}`)
	})
	mux.HandleFunc("/api/v1/models/"+ModelID+"/repo", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("FilePath")
		body, ok := files[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		downloads++
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestEnsureInstalledDownloadsVerifiesAndSkips(t *testing.T) {
	files := map[string]string{
		"model.safetensors": "SAFETENSORS-BODY",
		"unigram.json":      `{"unk_id":3,"vocab":[]}`,
		"config.json":       `{"hidden_size":384}`,
	}
	shas := map[string]string{"unigram.json": sha256Hex(`{"unk_id":3,"vocab":[]}`)} // one real, one omitted
	srv, downloads := fakeModelScope(t, files, shas)

	dir := filepath.Join(t.TempDir(), "model")
	in := &Installer{Dir: dir, BaseURL: srv.URL, HTTP: srv.Client()}
	m, err := in.EnsureInstalled(context.Background())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if *downloads != 3 {
		t.Fatalf("downloads = %d, want 3", *downloads)
	}
	for p, body := range files {
		got, rerr := os.ReadFile(filepath.Join(dir, p))
		if rerr != nil || string(got) != body {
			t.Fatalf("%s: %q (%v)", p, got, rerr)
		}
	}
	if m.Model != ModelID || m.Revision != ModelRevision || m.InstalledAt == "" {
		t.Fatalf("manifest: %+v", m)
	}
	// The manifest round-trips from disk with the verified sha recorded.
	back, rerr := ReadManifest(dir)
	if rerr != nil {
		t.Fatalf("read manifest: %v", rerr)
	}
	if back.Model != ModelID || back.Files["unigram.json"] != sha256Hex(files["unigram.json"]) {
		t.Fatalf("manifest round-trip: %+v", back)
	}
	// Second run: everything present with the right size → zero downloads.
	if _, err := in.EnsureInstalled(context.Background()); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if *downloads != 3 {
		t.Fatalf("reinstall must skip present files, downloads=%d", *downloads)
	}
	// A truncated file (wrong size) is re-downloaded.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := in.EnsureInstalled(context.Background()); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if *downloads != 4 {
		t.Fatalf("truncated file must be re-fetched, downloads=%d", *downloads)
	}
}

func TestEnsureInstalledRejectsShaMismatch(t *testing.T) {
	files := map[string]string{"model.safetensors": "BODY", "unigram.json": "{}", "config.json": "{}"}
	shas := map[string]string{"model.safetensors": strings.Repeat("a", 64)}
	srv, _ := fakeModelScope(t, files, shas)
	in := &Installer{Dir: t.TempDir(), BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := in.EnsureInstalled(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("sha mismatch must fail: %v", err)
	}
	// The bad file must not be left in place as "installed".
	if _, serr := os.Stat(filepath.Join(in.Dir, "model.safetensors")); serr == nil {
		t.Fatalf("mismatched weights must be removed")
	}
}

func TestEnsureInstalledRejectsMissingRequiredFile(t *testing.T) {
	srv, _ := fakeModelScope(t, map[string]string{"config.json": "{}"}, nil)
	in := &Installer{Dir: t.TempDir(), BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := in.EnsureInstalled(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lacks") {
		t.Fatalf("missing required files must fail: %v", err)
	}
}

func TestDefaultDirIsOurs(t *testing.T) {
	t.Setenv("CLUS_MODEL_DIR", "/x/own")
	t.Setenv("CLUS_MINILM_DIR", "/x/legacy")
	if got := DefaultDir(); got != "/x/own" {
		t.Fatalf("CLUS_MODEL_DIR wins: %s", got)
	}
	t.Setenv("CLUS_MODEL_DIR", "")
	if got := DefaultDir(); got != "/x/legacy" {
		t.Fatalf("legacy override: %s", got)
	}
	t.Setenv("CLUS_MINILM_DIR", "")
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, modelBaseDir, "models", ModelID)
	if got := DefaultDir(); got != want {
		t.Fatalf("default = %s, want %s", got, want)
	}
	if strings.Contains(DefaultDir(), "sirchmunk") {
		t.Fatalf("the suite must not live inside another project's cache: %s", DefaultDir())
	}
}
