package main

// Cluster browse REST face: the web workbench's cluster page reads these.
// List is namespace-scoped like every other face; detail carries the
// cluster's cite edges so the UI can render the cluster → source evidence
// links without a second round trip.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/affinity"
	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
)

// registerClusterFace mounts GET /v1/clusters, GET /v1/clusters/{id} and
// POST /v1/clusters/{id}/review — the review face is the workbench's
// 待复核 entry point: it re-validates a cluster's evidence windows against
// the CURRENT corpus (the same rune-exact rule the warm-reuse path applies)
// and promotes a valid emerging cluster to stable.
func registerClusterFace(mux *http.ServeMux, c cumulite.Port, st *ingest.Store, buckets *bucket.Store, serveNS, evidenceColl string) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if err := ns.Validate(r.URL.Query().Get("ns")); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		reqNS := firstNonEmpty(r.URL.Query().Get("ns"), serveNS)
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/clusters"), "/")
		if id != "" && strings.HasSuffix(id, "/review") {
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
				return
			}
			reviewCluster(w, r, c, st, reqNS, strings.TrimSuffix(id, "/review"), serveNS)
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
			return
		}
		store := cluster.NewCumuStore(c, ns.Coll(reqNS, "clus_clusters"))
		if id == "" {
			limit := 100
			if v := r.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
					limit = n
				}
			}
			all, err := store.All(r.Context())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			// Freshest first — the UI shows evolving knowledge, not archaeology.
			sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
			if want := r.URL.Query().Get("lifecycle"); want != "" {
				filtered := all[:0]
				for _, cl := range all {
					if cl.Lifecycle == want {
						filtered = append(filtered, cl)
					}
				}
				all = filtered
			}
			if len(all) > limit {
				all = all[:limit]
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"namespace": reqNS, "clusters": all,
			})
			return
		}
		cl, err := store.Get(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if cl == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "cluster not found"})
			return
		}
		// Cite edges anchored on this cluster (same namespace), so the detail
		// view can render evidence links in one response.
		cites := []map[string]any{}
		if all, cerr := deep.NewCumuCiteStore(c, ns.Coll(reqNS, "clus_cites")).List(r.Context()); cerr == nil {
			for _, e := range all {
				if from, _ := e["_from"].(string); from == id {
					cites = append(cites, e)
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"namespace": reqNS, "cluster": cl, "cites": cites,
		})
	}
	mux.HandleFunc("/v1/clusters", handler)
	mux.HandleFunc("/v1/clusters/", handler)

	// 学习状态面：GET /v1/learning → 各学习集计数 + clean 标志。验证与测试
	// 用它一请求确认“起点是干净的”（reset 在 CLI，HTTP 只读）。
	mux.HandleFunc("/v1/learning", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
			return
		}
		if err := ns.Validate(r.URL.Query().Get("ns")); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		reqNS := firstNonEmpty(r.URL.Query().Get("ns"), serveNS)
		// The evidence identity follows the sources-identity rule (see
		// storeForNS): the serve-level full identity in-namespace, the
		// request namespace's composite otherwise. The bare "clus_evidence"
		// this used to pass never matched a tenant's ns-prefixed list entry,
		// so the DEFAULT library's evidence was counted into the tenant's
		// report — a phantom not-clean on a fresh namespace.
		evidence := evidenceColl
		if reqNS != serveNS {
			evidence = ns.Coll(reqNS, "clus_evidence")
		}
		st, err := LearningState(r.Context(), c, reqNS, evidence)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	// 重学入口：POST /v1/learning/reset?ns= —— CLI `reset learned` 的 HTTP 面。
	// 防误触是双层的：注册库门禁 + 请求体 confirm 必须逐字等于库名（比 CLI 的
	// -yes 更强，因为它证明调用者知道自己在清哪个库）。语料不在清除集合里，
	// 由构造保证而非开关。
	mux.HandleFunc("/v1/learning/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
			return
		}
		reqNS := firstNonEmpty(r.URL.Query().Get("ns"), serveNS)
		if !requireHTTPBucket(w, r, buckets, reqNS) {
			return
		}
		var body struct {
			Confirm string `json:"confirm"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "json body with confirm field required"})
			return
		}
		if body.Confirm != reqNS {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "confirm mismatch",
				"hint":  "confirm 必须逐字等于要清空的库名（ns）",
			})
			return
		}
		// Evidence identity follows the same sources-identity rule as the GET
		// face above — resetting a tenant must clear the tenant's evidence,
		// not the default library's.
		evidence := evidenceColl
		if reqNS != serveNS {
			evidence = ns.Coll(reqNS, "clus_evidence")
		}
		rep, err := ResetLearned(r.Context(), c, reqNS, evidence, false)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})

	// 账本诊断面：GET /v1/affinity?token=宠物 → 该词元下衰减后的文档权重。
	// 只读，用于回答「系统到底学到了什么」。
	mux.HandleFunc("/v1/affinity", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
			return
		}
		if err := ns.Validate(r.URL.Query().Get("ns")); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		reqNS := firstNonEmpty(r.URL.Query().Get("ns"), serveNS)
		tokens := affinity.TrimTokens([]string{r.URL.Query().Get("token")}, 0)
		if len(tokens) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "token query param required"})
			return
		}
		weights, err := affinity.NewCumuStore(c, ns.Coll(reqNS, "clus_affinity")).Weights(r.Context(), tokens, time.Now())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		type row struct {
			SourceID string  `json:"source_id"`
			Weight   float64 `json:"weight"`
		}
		rows := make([]row, 0, len(weights))
		for id, wt := range weights {
			rows = append(rows, row{id, wt})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Weight > rows[j].Weight })
		if len(rows) > 50 {
			rows = rows[:50]
		}
		writeJSON(w, http.StatusOK, map[string]any{"namespace": reqNS, "token": tokens[0], "docs": rows})
	})
}

// reviewCluster re-validates one cluster's evidence against the live
// corpus. A cluster whose every window still pins back exactly is promoted
// emerging → stable (复核通过); one with broken pins stays emerging and the
// caller gets per-window reasons — the same self-heal the reuse path would
// apply on the next ask, now on demand instead of silently.
func reviewCluster(w http.ResponseWriter, r *http.Request, c cumulite.Port, st *ingest.Store, reqNS, id, serveNS string) {
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cluster id required"})
		return
	}
	store := cluster.NewCumuStore(c, ns.Coll(reqNS, "clus_clusters"))
	cl, err := store.Get(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if cl == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "cluster not found"})
		return
	}
	// Sources of the request's namespace: the cluster's evidence must pin
	// against the same corpus the searches read.
	src := st
	if src == nil || reqNS != serveNS {
		src = ingest.New(c, ns.Coll(reqNS, "clus_sources"), ns.Coll(reqNS, "clus_evidence"),
			ns.Coll(reqNS, "clus_clusters"), reqNS)
	}
	windows := cluster.NormalizeEvidence(cl.SourceID, cl.Evidence)
	ids := map[string]bool{}
	for _, ev := range windows {
		if ev.Source != "" {
			ids[ev.Source] = true
		}
	}
	byID := map[string]source.Source{}
	if len(ids) > 0 {
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		if got, gerr := src.SourcesByIDs(r.Context(), list); gerr == nil {
			for _, s := range got {
				byID[s.ID] = s
			}
		}
	}
	var reasons []string
	for _, ev := range windows {
		s, ok := byID[ev.Source]
		if !ok {
			reasons = append(reasons, ev.Source+"：源文档已不存在")
			continue
		}
		runes := []rune(s.Body)
		if ev.Start < 0 || ev.End > len(runes) || ev.Start >= ev.End {
			reasons = append(reasons, ev.Source+"：窗口越界")
			continue
		}
		if string(runes[ev.Start:ev.End]) != ev.Content {
			reasons = append(reasons, ev.Source+"：窗口与当前原文不一致（源已更新）")
		}
	}
	valid := len(reasons) == 0
	// A passing review stabilizes emerging clusters, and adjudicates
	// contested ones: the operator re-validated the evidence, so the cluster
	// re-enters the foldable population (the barrier EDGE stays — traversal
	// keeps refusing to cross a recorded conflict).
	if valid && (cl.Lifecycle == cluster.LifecycleEmerging || cl.Lifecycle == cluster.LifecycleContested) {
		cl.Lifecycle = cluster.LifecycleStable
		if serr := store.Save(r.Context(), *cl); serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"namespace": reqNS, "cluster_id": id,
		"valid": valid, "checked": len(windows), "reasons": reasons,
		"lifecycle": cl.Lifecycle,
	})
}
