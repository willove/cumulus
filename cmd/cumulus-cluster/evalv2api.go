package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/eval"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
)

// registerEvalV2Face mounts only explicitly selected registered namespaces.
// Legacy /v1/evals remains read-only and keeps its original response shapes.
func registerEvalV2Face(mux *http.ServeMux, c cumulite.Port, buckets *bucket.Store, service *eval.Service, serveNS, sourcesColl string) {
	mux.HandleFunc("/v1/eval/", func(w http.ResponseWriter, r *http.Request) {
		namespace := r.URL.Query().Get("ns")
		if !requireHTTPBucket(w, r, buckets, namespace) {
			return
		}
		route := strings.TrimPrefix(r.URL.Path, "/v1/eval/")
		fail := func(err error) {
			status := http.StatusBadRequest
			if errors.Is(err, eval.ErrNotFound) {
				status = http.StatusNotFound
			}
			if errors.Is(err, eval.ErrConflict) {
				status = http.StatusConflict
			}
			writeJSON(w, status, map[string]any{"error": evalSafeError(err)})
		}
		require := func(methods ...string) bool {
			for _, m := range methods {
				if r.Method == m {
					return true
				}
			}
			w.Header().Set("Allow", strings.Join(methods, ", "))
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return false
		}
		corpus := func() ([]source.Source, error) {
			coll := ns.Coll(namespace, "clus_sources")
			if namespace == serveNS && sourcesColl != "" {
				coll = sourcesColl
			}
			return evalCorpus(r.Context(), c, coll)
		}
		if route == "capabilities" {
			if !require(http.MethodGet) {
				return
			}
			model := "offline-keyword"
			if evalLiveAvailable() {
				model = envOr("LLM_CHAT_MODEL", "mimo/cascade-pro")
			}
			// queue_limit is the real backpressure boundary: a ninth pending run
			// is refused with 409, so the wizard can explain it before a user
			// submits instead of surfacing a bare conflict.
			writeJSON(w, http.StatusOK, map[string]any{"protocol": eval.Protocol, "live_available": evalLiveAvailable(), "model": model, "l1_available": true, "max_items": eval.MaxItems, "max_bytes": eval.MaxBytes, "concurrency": 1, "queue_limit": eval.QueueLimit, "defaults": eval.DefaultConfig()})
			return
		}
		if route == "datasets" || route == "datasets/validate" {
			if route == "datasets/validate" {
				if !require(http.MethodPost) {
					return
				}
			} else if !require(http.MethodGet, http.MethodPost) {
				return
			}
			if r.Method == http.MethodGet {
				datasets, err := service.Datasets(r.Context(), namespace)
				if err != nil {
					fail(err)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"datasets": datasets})
				return
			}
			var in struct {
				Name    string `json:"name"`
				Content string `json:"content"`
			}
			if err := decodeEval(r, &in); err != nil {
				fail(err)
				return
			}
			list, err := corpus()
			if err != nil {
				fail(err)
				return
			}
			if route == "datasets/validate" {
				writeJSON(w, http.StatusOK, eval.ValidateDataset(in.Content, list))
				return
			}
			d, v, err := service.SaveDataset(r.Context(), namespace, in.Name, in.Content, list)
			if err != nil {
				if !v.Valid {
					writeJSON(w, http.StatusBadRequest, v)
				} else {
					fail(err)
				}
				return
			}
			writeJSON(w, http.StatusCreated, d)
			return
		}
		if strings.HasPrefix(route, "datasets/") {
			if !require(http.MethodGet) {
				return
			}
			d, err := service.Dataset(r.Context(), namespace, strings.TrimPrefix(route, "datasets/"))
			if err != nil {
				fail(err)
				return
			}
			writeJSON(w, http.StatusOK, d)
			return
		}
		if route == "runs" {
			if !require(http.MethodGet, http.MethodPost) {
				return
			}
			if r.Method == http.MethodGet {
				runs, err := service.Runs(r.Context(), namespace)
				if err != nil {
					fail(err)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
				return
			}
			var in struct {
				DatasetID string      `json:"dataset_id"`
				Name      string      `json:"name"`
				RequestID string      `json:"request_id"`
				Config    eval.Config `json:"config"`
			}
			in.Config = eval.DefaultConfig()
			if err := decodeEval(r, &in); err != nil {
				fail(err)
				return
			}
			if err := in.Config.Validate(evalLiveAvailable(), true); err != nil {
				fail(err)
				return
			}
			list, err := corpus()
			if err != nil {
				fail(err)
				return
			}
			run, err := service.Start(r.Context(), namespace, in.DatasetID, in.Name, in.RequestID, in.Config, list)
			if err != nil {
				fail(err)
				return
			}
			writeJSON(w, http.StatusAccepted, run)
			return
		}
		if route == "compare" {
			if !require(http.MethodGet) {
				return
			}
			left, err := service.Get(r.Context(), namespace, r.URL.Query().Get("left"))
			if err != nil {
				fail(err)
				return
			}
			right, err := service.Get(r.Context(), namespace, r.URL.Query().Get("right"))
			if err != nil {
				fail(err)
				return
			}
			writeJSON(w, http.StatusOK, eval.CompareRuns(left.Run, right.Run, left.Results, right.Results))
			return
		}
		if strings.HasPrefix(route, "runs/") {
			parts := strings.Split(strings.TrimPrefix(route, "runs/"), "/")
			if len(parts) > 2 {
				http.NotFound(w, r)
				return
			}
			action := ""
			if len(parts) == 2 {
				action = parts[1]
			}
			if action == "cancel" || action == "retry" {
				if !require(http.MethodPost) {
					return
				}
				var run eval.Run
				var err error
				if action == "cancel" {
					run, err = service.Cancel(r.Context(), namespace, parts[0])
				} else {
					run, err = service.Retry(r.Context(), namespace, parts[0])
				}
				if err != nil {
					fail(err)
					return
				}
				writeJSON(w, http.StatusOK, run)
				return
			}
			if !require(http.MethodGet) {
				return
			}
			rec, err := service.Get(r.Context(), namespace, parts[0])
			if err != nil {
				fail(err)
				return
			}
			switch action {
			case "":
				writeJSON(w, http.StatusOK, rec.Run)
			case "items":
				writeJSON(w, http.StatusOK, map[string]any{"items": rec.Results})
			case "export":
				exportEval(w, r, rec)
			default:
				http.NotFound(w, r)
			}
			return
		}
		http.NotFound(w, r)
	})
}
func decodeEval(r *http.Request, v any) error {
	// JSON escaping may expand a 2 MiB JSONL string sixfold. The actual content
	// limit is checked separately; the envelope is bounded as well.
	const maxEnvelope = eval.MaxBytes*6 + 4096
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxEnvelope+1))
	if err != nil {
		return err
	}
	if len(raw) > maxEnvelope {
		return fmt.Errorf("evaluation request too large")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}
func evalCorpus(ctx context.Context, c cumulite.Port, coll string) ([]source.Source, error) {
	out := []source.Source{}
	bytes := 2
	for skip := 0; ; skip += 32 {
		res, err := c.Query(ctx, coll, contract.Query{Filter: map[string]any{"status": source.StatusActive}, Skip: skip, Limit: 32})
		if contract.IsNotFound(err) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		for _, d := range res.Documents {
			raw, err := json.Marshal(d)
			if err != nil {
				return nil, err
			}
			bytes += len(raw) + 1
			if bytes > eval.MaxSnapshotBytes || len(out) >= 10000 {
				return nil, fmt.Errorf("corpus snapshot limit exceeded (16 MiB or 10000 sources); use a smaller evaluation bucket")
			}
			var src source.Source
			if err := json.Unmarshal(raw, &src); err != nil {
				return nil, err
			}
			out = append(out, src)
		}
		if len(res.Documents) < 32 {
			break
		}
	}
	return out, nil
}
func exportEval(w http.ResponseWriter, r *http.Request, rec eval.Record) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	contentType := map[string]string{"json": "application/json", "jsonl": "application/x-ndjson", "csv": "text/csv"}[format]
	if contentType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "format must be json, jsonl or csv"})
		return
	}
	w.Header().Set("Content-Type", contentType+"; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="eval-`+rec.Run.ID+`.`+format+`"`)
	switch format {
	case "json":
		_ = json.NewEncoder(w).Encode(map[string]any{"run": rec.Run, "items": rec.Results, "questions": rec.Items, "corpus": rec.Corpus})
	case "jsonl":
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{"type": "run", "run": rec.Run})
		for _, it := range rec.Results {
			_ = enc.Encode(map[string]any{"type": "item", "run_id": rec.Run.ID, "item": it})
		}
	case "csv":
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"run_id", "protocol", "config_text", "items_sha", "corpus_sha", "config_sha", "id", "query", "reference", "answer", "state", "error", "rule_match", "evidence_hit", "citation_resolved", "judge_correct", "closed_book_match", "search_tokens", "judge_tokens", "closed_book_tokens", "latency_ms", "attempt", "citations", "attempts"})
		boolText := func(b *bool) string {
			if b == nil {
				return ""
			}
			return strconv.FormatBool(*b)
		}
		for _, it := range rec.Results {
			cites, _ := json.Marshal(it.Citations)
			attempts, _ := json.Marshal(it.Attempts)
			row := []string{rec.Run.ID, rec.Run.Protocol, rec.Run.ConfigText, rec.Run.Frozen.ItemsSHA, rec.Run.Frozen.CorpusSHA, rec.Run.Frozen.ConfigSHA, it.ID, it.Query, it.Reference, it.Answer, it.State, it.Error, strconv.FormatBool(it.RuleMatch), strconv.FormatBool(it.EvidenceHit), strconv.FormatBool(it.CitationResolved), boolText(it.JudgeCorrect), boolText(it.ClosedBookMatch), strconv.FormatInt(it.SearchTokens, 10), strconv.FormatInt(it.JudgeTokens, 10), strconv.FormatInt(it.ClosedBookTokens, 10), strconv.FormatInt(it.LatencyMS, 10), strconv.Itoa(it.Attempt), string(cites), string(attempts)}
			for i := range row {
				row[i] = eval.SafeCSV(row[i])
			}
			_ = cw.Write(row)
		}
		cw.Flush()
	}
}
