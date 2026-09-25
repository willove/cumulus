package main

// Model REST face: the workbench 配置 page reads weight status, starts the
// ModelScope download (async — 464MB must not hold a request open), and
// verifies the installed weights by running them. GET /v1/config exposes the
// same masked endpoint config as `cumulus-cluster env`.

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
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
		if r.Method == http.MethodPost {
			api.saveConfig(w, r)
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET/POST only"})
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

	mux.HandleFunc("/v1/config/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
			return
		}
		ps := newProdStack()
		if ps.chat == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "当前为离线规则模式（未配置端点），无法测试连接",
				"hint":  "先填 Base URL 与 API Key 并保存",
			})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		before := ps.chat.TotalTokens()
		started := time.Now()
		answer, err := ps.chat.Complete(ctx, "只回复两个字：可用")
		report := map[string]any{
			"ok":         err == nil,
			"model":      os.Getenv("AIGATE_CHAT_MODEL"),
			"latency_ms": time.Since(started).Milliseconds(),
			"tokens":     ps.chat.TotalTokens() - before,
		}
		if err != nil {
			report["error"] = evalSafeError(err)
			writeJSON(w, http.StatusBadGateway, report)
			return
		}
		report["answer"] = strings.TrimSpace(answer)
		writeJSON(w, http.StatusOK, report)
	})
}

// saveConfig persists the operator's endpoint/model settings and applies them to
// the running process. Two properties matter more than the feature itself:
// the API key is never echoed back (Sirchmunk returns it in cleartext and relies
// on an input type=password for cover), and the write goes through .env rather
// than only os.Setenv, so a restart keeps the configuration.
func (api *modelAPI) saveConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BaseURL        *string `json:"base_url"`
		ChatModel      *string `json:"chat_model"`
		EmbedModel     *string `json:"embed_model"`
		APIKey         *string `json:"api_key"`
		ReasoningSplit *bool   `json:"reasoning_split"`
	}
	if err := decode(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	file, env := map[string]string{}, map[string]string{}
	add := func(llmKey, agateKey, value string) {
		if llmKey != "" {
			file[llmKey] = value
		}
		file[agateKey] = value
		env[agateKey] = value
		if llmKey != "" {
			env[llmKey] = value
		}
	}
	if in.BaseURL != nil {
		value := strings.TrimSpace(*in.BaseURL)
		if value != "" {
			parsed, err := url.Parse(value)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Base URL 必须是带主机名的 http(s) 地址"})
				return
			}
		}
		// 空字符串是「清空」的显式请求（回到离线桩），不是「不修改」。
		add("LLM_BASE_URL", "AIGATE_BASE_URL", value)
	}
	if in.ChatModel != nil {
		value := strings.TrimSpace(*in.ChatModel)
		if len(value) > 200 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Chat 模型名过长"})
			return
		}
		add("LLM_MODEL_NAME", "AIGATE_CHAT_MODEL", value)
	}
	if in.EmbedModel != nil {
		value := strings.TrimSpace(*in.EmbedModel)
		if len(value) > 200 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Embed 模型名过长"})
			return
		}
		// 嵌入模型没有 LLM_* 别名，只有 AIGATE_EMBED_MODEL。
		add("", "AIGATE_EMBED_MODEL", value)
	}
	if in.APIKey != nil {
		value := strings.TrimSpace(*in.APIKey)
		if len(value) > 2000 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "API Key 过长"})
			return
		}
		// 留空或 "***" = 不修改。密钥从不回显，所以界面拿不到真值，只能这样表达。
		if value != "" && value != "***" {
			add("LLM_API_KEY", "AIGATE_API_KEY", value)
		}
	}
	if in.ReasoningSplit != nil {
		value := "0"
		if *in.ReasoningSplit {
			value = "1"
		}
		add("", "AIGATE_REASONING_SPLIT", value)
	}
	if len(file) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "没有需要修改的字段"})
		return
	}
	path := envFilePath()
	if err := writeEnvValues(path, file); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 热生效：新请求会重新构建检索栈，因此保存后无需重启即可用于下一次检索。
	for key, value := range env {
		_ = os.Setenv(key, value)
	}
	saved := make([]string, 0, len(env))
	for key := range env {
		saved = append(saved, key)
	}
	sort.Strings(saved)
	log.Printf("[config] saved %d value(s) to %s (hot-applied)", len(file), path)
	writeJSON(w, http.StatusOK, map[string]any{
		"saved":       saved,
		"env_file":    path,
		"hot_applied": true,
		"note":        "已写入 " + path + " 并对当前进程生效；下次检索即使用新配置。",
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
