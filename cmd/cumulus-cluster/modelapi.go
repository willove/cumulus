package main

// Model REST face: the workbench 配置 page reads weight status, starts the
// ModelScope download (async — 464MB must not hold a request open), and
// verifies the installed weights by running them. GET /v1/config exposes the
// same masked endpoint config as `cumulus-cluster env`.

import (
	"context"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/willove/cumulus/internal/minilm"
)

// modelAPI owns the serve process's install state.
type modelAPI struct {
	mu         sync.Mutex
	installing bool
	progress   minilm.Progress
	err        string
	finishedAt time.Time
}

func (a *modelAPI) snapshot() (bool, minilm.Progress, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.installing, a.progress, a.err
}

func (a *modelAPI) start() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.installing {
		return false
	}
	a.installing = true
	a.progress = minilm.Progress{}
	a.err = ""
	return true
}

func (a *modelAPI) finish(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.installing = false
	a.finishedAt = time.Now()
	if err != nil {
		a.err = err.Error()
	}
}

// modelDownloadBudget bounds the async weight download (default 2h, ample for
// ~485MB; overridable for slow links).
func modelDownloadBudget() time.Duration {
	if v := os.Getenv("CLUS_MODEL_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 2 * time.Hour
}

// registerModelFace mounts GET /v1/model, POST /v1/model/install,
// POST /v1/model/verify and GET /v1/config.
func registerModelFace(mux *http.ServeMux) {
	api := &modelAPI{}

	mux.HandleFunc("/v1/model", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			installing, progress, errMsg := api.snapshot()
			st := collectModelStatus(minilm.DefaultDir())
			writeJSON(w, http.StatusOK, map[string]any{
				"installed": st.Installed, "dir": st.Dir, "dims": st.Dims,
				"files": st.Files, "manifest": st.Manifest,
				"installing": installing, "progress": progress, "error": errMsg,
				"model_id": minilm.ModelID, "source": minilm.ModelScopeBase,
				"size_hint_mb": 485,
			})
		case http.MethodPost:
			// POST /v1/model starts the download (idempotent: installed → no-op).
			if minilm.Available() {
				writeJSON(w, http.StatusOK, map[string]any{"installed": true, "dir": minilm.DefaultDir()})
				return
			}
			if !api.start() {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "install already running"})
				return
			}
			// A cancellable, bounded context: the download must not outlive the
			// process forever, and a stuck transfer must not hang a goroutine
			// that no request is waiting on. 485MB over a slow link is the
			// budget; CLUS_MODEL_TIMEOUT overrides it.
			bg, cancel := context.WithTimeout(context.Background(), modelDownloadBudget())
			go func() {
				defer cancel()
				_, err := installModel(bg, minilm.DefaultDir(), func(p minilm.Progress) {
					api.mu.Lock()
					api.progress = p
					api.mu.Unlock()
				})
				api.finish(err)
				if err == nil {
					log.Printf("[model] weights installed at %s", minilm.DefaultDir())
				} else {
					log.Printf("[model] install failed: %v", err)
				}
			}()
			writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "dir": minilm.DefaultDir()})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET/POST only"})
		}
	})

	mux.HandleFunc("/v1/model/verify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
			return
		}
		rep, err := verifyModel(r.Context(), minilm.DefaultDir())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})

	mux.HandleFunc("/v1/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
			return
		}
		key := os.Getenv("AIGATE_API_KEY")
		// Resolve through the production wiring, without calling a model or
		// installing weights. Configured values alone hide offline overrides,
		// MiniLM fallback and AIGATE_REASONING_SPLIT=0/1.
		ps := newProdStack()
		effectiveEmbedder, embedErr := embedderName(ps.emb), ""
		if ps.embErr != nil {
			effectiveEmbedder, embedErr = "", ps.embErr.Error()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"base_url":           os.Getenv("AIGATE_BASE_URL"),
			"chat_model":         os.Getenv("AIGATE_CHAT_MODEL"),
			"embed_model":        os.Getenv("AIGATE_EMBED_MODEL"),
			"api_key_set":        key != "",
			"api_key_len":        len(key),
			"reasoning_split":    ps.chat != nil && ps.chat.ReasoningSplit,
			"minilm_required":    minilm.Required(),
			"offline":            ps.chat == nil,
			"effective_embedder": effectiveEmbedder,
			"embedder_error":     embedErr,
		})
	})
}

// logModelReminder is the serve-boot weight check: a non-interactive process
// can only point at the workbench 配置 page (or the CLI).
func logModelReminder() {
	if os.Getenv("CLUS_EMBED") != "minilm" || minilm.Available() {
		return
	}
	log.Printf("[model] MiniLM 权重缺席：%s —— 工作台「配置」页可下载，或 cumulus-cluster model install", minilm.DefaultDir())
}
