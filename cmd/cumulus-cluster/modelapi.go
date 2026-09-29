package main

// Model REST face: the workbench 配置 page reads weight status, starts the
// ModelScope download (async — 464MB must not hold a request open), and
// verifies the installed weights by running them. GET /v1/config exposes the
// same masked endpoint config as `cumulus-cluster env`.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/minilm"
	"github.com/willove/cumulus/internal/modelprofile"
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
// POST /v1/model/verify, GET/POST /v1/config, the model-profile registry
// (/v1/models*) and the usage ledger (/v1/usage).
func registerModelFace(mux *http.ServeMux, c cumulite.Port) {
	api := &modelAPI{}
	profiles := modelprofile.New(c)

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
		// 可选 body {profile_id}：按已保存（未必激活）的 profile 建临时客户端
		// 试连——激活前先验证，坏配置不进 .env。
		var in struct {
			ProfileID string `json:"profile_id"`
		}
		_ = decode(r, &in)
		var client *llm.ChatClient
		model := os.Getenv("AIGATE_CHAT_MODEL")
		if in.ProfileID != "" {
			p, err := profiles.Get(r.Context(), in.ProfileID)
			if err != nil || p == nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "profile not found: " + in.ProfileID})
				return
			}
			client = &llm.ChatClient{BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.ChatModel, HTTPClient: newPooledHTTPClient(defaultScorerWorkers()), ReasoningSplit: p.ReasoningSplit}
			model = p.ChatModel
		} else {
			ps := newProdStack()
			if ps.chat == nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "当前为离线规则模式（未配置端点），无法测试连接",
					"hint":  "先填 Base URL 与 API Key 并保存",
				})
				return
			}
			client = ps.chat
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		before := client.TotalTokens()
		started := time.Now()
		answer, err := client.Complete(ctx, "只回复两个字：可用")
		report := map[string]any{
			"ok":         err == nil,
			"model":      model,
			"latency_ms": time.Since(started).Milliseconds(),
			"tokens":     client.TotalTokens() - before,
		}
		if err != nil {
			report["error"] = evalSafeError(err)
			writeJSON(w, http.StatusBadGateway, report)
			return
		}
		report["answer"] = strings.TrimSpace(answer)
		writeJSON(w, http.StatusOK, report)
	})

	// ── 模型配置档案：多条端点配置 + 激活一条。激活 = 物化（写 .env +
	// os.Setenv），下一请求即用——栈每请求重建，天然热切。key 只存本地
	// store、接口永不回显（与 /v1/config 同约）。 ──
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			list, err := profiles.List(r.Context())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			active, _ := profiles.ActiveID(r.Context())
			masked := make([]map[string]any, 0, len(list))
			for _, p := range list {
				masked = append(masked, maskedProfile(p))
			}
			writeJSON(w, http.StatusOK, map[string]any{"profiles": masked, "active": active})
		case http.MethodPost:
			var p modelprofile.Profile
			if err := decode(r, &p); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			p.ID = strings.TrimSpace(p.ID)
			p.Label = strings.TrimSpace(p.Label)
			p.BaseURL = strings.TrimSpace(p.BaseURL)
			if parsed, err := url.Parse(p.BaseURL); p.BaseURL == "" || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Base URL 必须是带主机名的 http(s) 地址"})
				return
			}
			if len(p.ChatModel) > 200 || len(p.EmbedModel) > 200 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "模型名过长"})
				return
			}
			saved, err := profiles.Save(r.Context(), p)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			log.Printf("[models] saved profile %s (%s)", saved.ID, saved.Label)
			writeJSON(w, http.StatusOK, maskedProfile(saved))
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET/POST only"})
		}
	})
	mux.HandleFunc("/v1/models/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/models"), "/")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "profile id required"})
			return
		}
		// activate 自带一层路径（/v1/models/{id}/activate），先剥掉再校验。
		if r.Method == http.MethodPost && strings.HasSuffix(id, "/activate") {
			name := strings.TrimSuffix(id, "/activate")
			if name == "" || strings.Contains(name, "/") {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "profile id required"})
				return
			}
			p, err := profiles.Get(r.Context(), name)
			if err != nil || p == nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "profile not found: " + name})
				return
			}
			path, err := materializeProfile(*p)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if err := profiles.SetActive(r.Context(), name); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			log.Printf("[models] activated profile %s (%s) → %s (hot-applied)", p.ID, p.Label, path)
			writeJSON(w, http.StatusOK, map[string]any{"activated": name, "hot_applied": true, "env_file": path})
			return
		}
		if strings.Contains(id, "/") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "profile id required"})
			return
		}
		if r.Method != http.MethodDelete {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "DELETE or POST /activate only"})
			return
		}
		if err := profiles.Remove(r.Context(), id); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": id})
	})

	// ── 消费台账：每次检索一条持久记录，按模型/库可追溯。 ──
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
			return
		}
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		wantModel := r.URL.Query().Get("model")
		wantNS := r.URL.Query().Get("ns")
		records, scanned, err := readUsage(r.Context(), c, limit, wantModel, wantNS)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"records": records, "scanned": scanned})
	})
}

// maskedProfile strips the API key and reports its presence the /v1/config way.
func maskedProfile(p modelprofile.Profile) map[string]any {
	return map[string]any{
		"id": p.ID, "label": firstNonEmpty(p.Label, p.ID), "base_url": p.BaseURL,
		"chat_model": p.ChatModel, "embed_model": p.EmbedModel,
		"api_key_set": p.APIKey != "", "api_key_len": len(p.APIKey),
		"reasoning_split": p.ReasoningSplit,
		"created_at": p.CreatedAt, "updated_at": p.UpdatedAt, "last_used_at": p.LastUsedAt,
	}
}

// materializeProfile writes one profile through to .env (restart-durable) and
// the process env (hot). Same alias discipline as saveConfig: LLM_* + AIGATE_*.
func materializeProfile(p modelprofile.Profile) (string, error) {
	rs := "0"
	if p.ReasoningSplit {
		rs = "1"
	}
	file := map[string]string{
		"LLM_BASE_URL": p.BaseURL, "AIGATE_BASE_URL": p.BaseURL,
		"LLM_MODEL_NAME": p.ChatModel, "AIGATE_CHAT_MODEL": p.ChatModel,
		"AIGATE_EMBED_MODEL":     p.EmbedModel,
		"AIGATE_REASONING_SPLIT": rs,
	}
	if p.APIKey != "" {
		file["LLM_API_KEY"] = p.APIKey
		file["AIGATE_API_KEY"] = p.APIKey
	}
	path := envFilePath()
	if err := writeEnvValues(path, file); err != nil {
		return "", err
	}
	for key, value := range file {
		_ = os.Setenv(key, value)
	}
	return path, nil
}

// usageCollection is the persistent consumption ledger. One document per
// finished search; the engine page reads it back per model / per library.
const usageCollection = "clus_usage"

// usageScanCap bounds the ledger read: the recent window is what the operator
// audits; deep history belongs to an export, not a UI page load.
const usageScanCap = 4000

func recordConsumption(ctx context.Context, c cumulite.Port, namespace, model string, prompt, completion, total int64, res deep.Result) {
	if c == nil {
		return
	}
	doc := map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339Nano),
		"ns": namespace, "model": model,
		"tokens": total, "prompt_tokens": prompt, "completion_tokens": completion,
		"mode": res.Mode, "reused": res.Reused, "latency_ms": res.LatencyMS,
	}
	_, _ = c.Insert(ctx, usageCollection, []map[string]any{doc})
}

func readUsage(ctx context.Context, c cumulite.Port, limit int, wantModel, wantNS string) ([]map[string]any, int, error) {
	out := []map[string]any{}
	scanned := 0
	var skip int
	for scanned < usageScanCap {
		res, err := c.Query(ctx, usageCollection, contract.Query{Limit: 500, Skip: skip})
		if err != nil {
			if contract.IsNotFound(err) {
				break
			}
			return nil, scanned, err
		}
		if len(res.Documents) == 0 {
			break
		}
		scanned += len(res.Documents)
		skip += len(res.Documents)
		for _, d := range res.Documents {
			if wantModel != "" && d["model"] != wantModel {
				continue
			}
			if wantNS != "" && d["ns"] != wantNS {
				continue
			}
			out = append(out, d)
		}
		if len(res.Documents) < 500 {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["at"]) > fmt.Sprint(out[j]["at"])
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, scanned, nil
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
