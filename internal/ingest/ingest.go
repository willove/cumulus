// Package ingest is the dual-path write side of the ask suite: content-addressed
// upsert of source documents, explicit staleness on update, tombstone delete,
// and a resumable batch job. Index materialization (embeddings) is out of band
// — put never blocks on a model.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cumubase/ask/internal/ns"
	"github.com/cumubase/ask/internal/source"
	"github.com/cumubase/cumudb/pkg/client"
)

// Result reports what a single put did.
type Result struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // created | updated | unchanged
	Version int    `json:"version"`
	Digest  string `json:"digest"`
	StaleID string `json:"stale_id,omitempty"`
}

// Store writes ask_sources and ask_evidence against cumudb. Every identity it
// touches is passed in already scoped (composite "ns:coll" identity for a
// tenant, bare name for the default library); namespace only scopes the flat
// KV keys the Store owns (job cursors, reconcile cursor) via ns.KV.
type Store struct {
	c         *client.Client
	sources   string
	evidence  string
	clusters  string
	namespace string
	jobs      string
}

func New(c *client.Client, sources, evidence, clusters, namespace string) *Store {
	if sources == "" {
		sources = "ask_sources"
	}
	if evidence == "" {
		evidence = "ask_evidence"
	}
	if clusters == "" {
		clusters = "ask_clusters"
	}
	return &Store{
		c: c, sources: sources, evidence: evidence, clusters: clusters,
		namespace: namespace, jobs: ns.KV(namespace, "ask:job:"),
	}
}

// MaxSyncBodyBytes is the synchronous put cap (§3.4.2): larger corpora must
// go through the Job path (ingest-files / serve jobs), never block a request.
const MaxSyncBodyBytes = 256 << 10

// Put upserts one source. Same digest → unchanged. New digest under the same
// business key → version+1, previous content marked stale.
func (s *Store) Put(ctx context.Context, src source.Source) (Result, error) {
	if src.Body == "" {
		return Result{}, fmt.Errorf("ingest: body is required")
	}
	if len(src.Body) > MaxSyncBodyBytes {
		return Result{}, fmt.Errorf("ingest: body is %d bytes, over the %d synchronous cap — use ingest-files or the serve job path", len(src.Body), MaxSyncBodyBytes)
	}
	now := time.Now().UTC()
	src.Digest = source.Digest(src.Body)
	if src.Status == "" {
		src.Status = source.StatusActive
	}

	prev, err := s.findByBusiness(ctx, src.BusinessKey, src.Title)
	if err != nil {
		return Result{}, err
	}

	if prev != nil && prev.Digest == src.Digest && prev.Status == source.StatusActive {
		return Result{ID: prev.ID, Status: "unchanged", Version: prev.Version, Digest: prev.Digest}, nil
	}

	if prev == nil {
		src.ID = source.IDFor(src.Body)
		src.Version = 1
		src.IngestedAt = now
		src.UpdatedAt = now
		if _, err := s.c.Insert(ctx, s.sources, []map[string]any{toDoc(src)}); err != nil {
			if existing, gerr := s.getSource(ctx, src.ID); gerr == nil && existing != nil {
				return Result{ID: existing.ID, Status: "unchanged", Version: existing.Version, Digest: existing.Digest}, nil
			}
			return Result{}, err
		}
		return Result{ID: src.ID, Status: "created", Version: 1, Digest: src.Digest}, nil
	}

	staleID := prev.ID
	src.ID = source.IDFor(src.Body)
	src.Version = prev.Version + 1
	src.IngestedAt = prev.IngestedAt
	src.UpdatedAt = now
	if src.BusinessKey == "" {
		src.BusinessKey = prev.BusinessKey
	}
	if _, err := s.c.Insert(ctx, s.sources, []map[string]any{toDoc(src)}); err != nil {
		return Result{}, err
	}
	if _, err := s.c.PatchDocument(ctx, s.sources, staleID, map[string]any{
		"$set": map[string]any{
			"status":     source.StatusStale,
			"updated_at": now.Format(time.RFC3339Nano),
		},
	}); err != nil {
		return Result{}, fmt.Errorf("marking stale %s: %w", staleID, err)
	}
	if _, err := s.invalidateEvidence(ctx, staleID); err != nil {
		return Result{}, err
	}
	return Result{ID: src.ID, Status: "updated", Version: src.Version, Digest: src.Digest, StaleID: staleID}, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.c.PatchDocument(ctx, s.sources, id, map[string]any{
		"$set": map[string]any{
			"status":     source.StatusDeleted,
			"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
		},
	}); err != nil {
		return fmt.Errorf("ingest delete %s: %w", id, err)
	}
	_, err := s.invalidateEvidence(ctx, id)
	return err
}

func (s *Store) Get(ctx context.Context, id string) (*source.Source, error) {
	return s.getSource(ctx, id)
}

// ActiveSources lists live sources — the L0 sampling candidate set.
func (s *Store) ActiveSources(ctx context.Context) ([]source.Source, error) {
	// Paginate: the corpus may exceed any single page (14k+ articles), and a
	// silently truncated candidate universe breaks every ranking face.
	const page = 1000
	var out []source.Source
	for skip := 0; ; skip += page {
		res, err := s.c.Query(ctx, s.sources, client.Query{
			Filter: map[string]any{"status": source.StatusActive},
			Skip:   skip,
			Limit:  page,
		})
		if err != nil {
			return nil, err
		}
		for _, d := range res.Documents {
			src, err := fromDoc(d)
			if err != nil {
				return nil, err
			}
			out = append(out, *src)
		}
		if len(res.Documents) < page {
			break
		}
	}
	return out, nil
}

// EvidenceHit is one live evidence window (ask_evidence) — the "history
// success" input to the LENS B4 prior's history arm.
type EvidenceHit struct {
	SourceID string  `json:"source_id"`
	Score    float64 `json:"score"`
	Snippet  string  `json:"snippet"`
}

// SourcesByIDs returns the ACTIVE sources with the given ids (unknown or
// retired ids are skipped). G2: the reuse path validates a warm prior against
// only the documents a cluster anchors on, instead of reading the whole
// corpus (14k articles ≈ 2s of paged reads for a 0-sample answer).
func (s *Store) SourcesByIDs(ctx context.Context, ids []string) ([]source.Source, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	const page = 500
	var out []source.Source
	for start := 0; start < len(ids); start += page {
		end := start + page
		if end > len(ids) {
			end = len(ids)
		}
		res, err := s.c.Query(ctx, s.sources, client.Query{
			Filter: map[string]any{"_id": map[string]any{"$in": ids[start:end]}},
			Limit:  page,
		})
		if err != nil {
			return out, err
		}
		for _, d := range res.Documents {
			src, err := fromDoc(d)
			if err != nil {
				return out, err
			}
			if src.Status == source.StatusActive {
				out = append(out, *src)
			}
		}
	}
	return out, nil
}

// ActiveEvidence returns live evidence windows scored at or above minScore,
// newest first, capped at limit. Read-only; failures degrade to nil so the
// prior's history arm simply stays empty.
func (s *Store) ActiveEvidence(ctx context.Context, minScore float64, limit int) ([]EvidenceHit, error) {
	if limit <= 0 {
		limit = 200
	}
	if minScore <= 0 {
		minScore = 7
	}
	res, err := s.c.Query(ctx, s.evidence, client.Query{
		Filter: map[string]any{"status": "live", "score": map[string]any{"$gte": minScore}},
		Limit:  limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]EvidenceHit, 0, len(res.Documents))
	for _, d := range res.Documents {
		id, _ := d["doc_id"].(string)
		sc, _ := d["score"].(float64)
		snip, _ := d["snippet"].(string)
		if id == "" {
			continue
		}
		out = append(out, EvidenceHit{SourceID: id, Score: sc, Snippet: snip})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// MarkEvidence records one evidence window. Non-active sources are rejected.
func (s *Store) MarkEvidence(ctx context.Context, docID string, start, end int, score float64, reasoning, snippet string) (string, error) {
	src, err := s.getSource(ctx, docID)
	if err != nil {
		return "", err
	}
	if src == nil || src.Status != source.StatusActive {
		return "", fmt.Errorf("ingest: source %s not active", docID)
	}
	id := fmt.Sprintf("ev:%s:%d:%d", docID[4:], start, end)
	doc := map[string]any{
		"_id":       id,
		"doc_id":    docID,
		"start":     start,
		"end":       end,
		"score":     score,
		"reasoning": reasoning,
		"snippet":   snippet,
		"status":    "live",
		"created":   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if _, err := s.c.Insert(ctx, s.evidence, []map[string]any{doc}); err != nil {
		if existing, gerr := s.getEvidence(ctx, id); gerr == nil && existing != nil {
			return id, nil
		}
		return "", err
	}
	return id, nil
}

// IngestJSONL upserts a batch with a resumable cursor under the job key.
func (s *Store) IngestJSONL(ctx context.Context, jobKey string, records []map[string]any, mapFn func(map[string]any) (source.Source, error)) (int, error) {
	if jobKey == "" {
		jobKey = "default"
	}
	cursorKey := s.jobs + jobKey
	start := 0
	if raw, err := s.c.KVGet(ctx, cursorKey); err == nil && len(raw) > 0 {
		if n, err := strconv.Atoi(string(raw)); err == nil {
			start = n
		}
	}
	done := 0
	for i := start; i < len(records); i++ {
		src, err := mapFn(records[i])
		if err != nil {
			return done, fmt.Errorf("record %d: %w", i, err)
		}
		if _, err := s.Put(ctx, src); err != nil {
			return done, fmt.Errorf("record %d put: %w", i, err)
		}
		done++
		if err := s.c.KVPut(ctx, cursorKey, []byte(strconv.Itoa(i+1)), 0); err != nil {
			return done, err
		}
	}
	return done, nil
}

// Ensure declares the suite's collections (idempotent) — D7: collection shape
// is declared once (ensure/scenario), never per ingest. ask_sources records a
// changelog: downstream reconciliation (Reconcile) consumes it.
func (s *Store) Ensure(ctx context.Context, extra ...string) ([]string, error) {
	colls := []string{s.sources, s.evidence, s.clusters}
	colls = append(colls, extra...)
	seen := map[string]bool{}
	out := make([]string, 0, len(colls))
	for _, c := range colls {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if err := s.c.EnsureCollection(ctx, c); err != nil {
			return out, fmt.Errorf("ingest ensure %s: %w", c, err)
		}
		out = append(out, c)
	}
	if err := s.c.SetChangelog(ctx, s.sources, true); err != nil {
		return out, fmt.Errorf("ingest ensure changelog on %s: %w", s.sources, err)
	}
	return out, nil
}

// ReconcileReport summarizes one downstream pass over the ask_sources
// changelog (§3.4.2: 下游轮询消费，游标持久化，at-least-once + _id 幂等).
type ReconcileReport struct {
	Scanned        int    `json:"scanned"`
	EvInvalidated  int    `json:"evidence_invalidated"`
	ClustersMarked int    `json:"clusters_marked"`
	Cursor         uint64 `json:"cursor"`
}

// Reconcile consumes ask_sources changes since the persisted cursor: sources
// retired out-of-band (stale/deleted written without going through Put) get
// their live evidence invalidated and clusters anchored on them marked 待复核.
// Every action is idempotent, so at-least-once delivery is safe.
func (s *Store) Reconcile(ctx context.Context) (ReconcileReport, error) {
	rep := ReconcileReport{}
	cursorKey := ns.KV(s.namespace, "ask:reconcile:"+s.sources)
	cur := uint64(0)
	if raw, err := s.c.KVGet(ctx, cursorKey); err == nil && len(raw) > 0 {
		cur, _ = strconv.ParseUint(string(raw), 10, 64)
	}
	rep.Cursor = cur
	for {
		page, err := s.c.Changes(ctx, s.sources, cur, 200)
		if err != nil {
			return rep, err
		}
		for _, ch := range page.Changes {
			rep.Scanned++
			src, err := s.getSource(ctx, ch.ID)
			if err != nil || src == nil {
				continue // physically gone: nothing left to reconcile
			}
			if src.Status == source.StatusActive {
				continue // live write: put already handled its downstream
			}
			if n, err := s.invalidateEvidence(ctx, ch.ID); err == nil {
				rep.EvInvalidated += n
			}
			if s.markClustersStale(ctx, ch.ID) {
				rep.ClustersMarked++
			}
		}
		cur = page.Cursor
		rep.Cursor = cur
		if err := s.c.KVPut(ctx, cursorKey, []byte(strconv.FormatUint(cur, 10)), 0); err != nil {
			return rep, err
		}
		if page.Count == 0 || len(page.Changes) == 0 {
			break
		}
	}
	return rep, nil
}

// markClustersStale flags clusters anchored on a retired source 待复核
// (emerging) — the reconcile-side trigger for the B8 re-validation.
func (s *Store) markClustersStale(ctx context.Context, docID string) bool {
	res, err := s.c.Query(ctx, s.clusters, client.Query{
		Filter: map[string]any{"source_id": docID},
		Limit:  1000,
	})
	if err != nil {
		return false
	}
	marked := false
	for _, d := range res.Documents {
		id, _ := d["_id"].(string)
		if id == "" || d["lifecycle"] == "emerging" {
			continue
		}
		if _, err := s.c.PatchDocument(ctx, s.clusters, id, map[string]any{
			"$set": map[string]any{"lifecycle": "emerging"},
		}); err == nil {
			marked = true
		}
	}
	return marked
}

// Reclaim physically removes retired sources ("保留是决策不是副作用"):
// tombstoned (deleted) always; stale revisions only with includeStale.
func (s *Store) Reclaim(ctx context.Context, includeStale bool) (int, error) {
	statuses := []string{source.StatusDeleted}
	if includeStale {
		statuses = append(statuses, source.StatusStale)
	}
	res, err := s.c.Query(ctx, s.sources, client.Query{
		Filter: map[string]any{"status": map[string]any{"$in": statuses}},
		Limit:  1000,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range res.Documents {
		id, _ := d["_id"].(string)
		if id == "" {
			continue
		}
		if err := s.purgeEvidence(ctx, id); err != nil {
			return n, err
		}
		if ok, err := s.c.DeleteDocument(ctx, s.sources, id); err != nil {
			return n, err
		} else if ok {
			n++
		}
	}
	return n, nil
}

func (s *Store) purgeEvidence(ctx context.Context, docID string) error {
	res, err := s.c.Query(ctx, s.evidence, client.Query{
		Filter: map[string]any{"doc_id": docID},
		Limit:  1000,
	})
	if err != nil {
		return err
	}
	for _, d := range res.Documents {
		id, _ := d["_id"].(string)
		if id == "" {
			continue
		}
		if _, err := s.c.DeleteDocument(ctx, s.evidence, id); err != nil {
			return err
		}
	}
	return nil
}

// EmbedderFn embeds texts (aigate in production, cluster.Local offline).
type EmbedderFn func(ctx context.Context, texts []string) ([][]float64, error)

// EnsureEmbed declares the body-embed vector index (the L1 cache, D1/D7) and
// backfills missing vectors in batches. It is the only path that writes
// content vectors; put never blocks on a model. No embedder → no-op.
func (s *Store) EnsureEmbed(ctx context.Context, embed EmbedderFn, dims int, model string, batch int) (n int, err error) {
	if embed == nil {
		return 0, nil
	}
	if err := s.c.CreateIndexRequest(ctx, s.sources, client.IndexRequest{
		Name: "ask_body_embed", Field: "body_embed", Type: "vector",
		Dims: dims, Metric: "cosine", Model: model,
	}); err != nil && !strings.Contains(err.Error(), "INDEX_EXISTS") {
		// Ensure semantics: an existing index (created by a prior run or the
		// search -l1pre path) is success, not a conflict.
		return 0, fmt.Errorf("ingest ensure body_embed index: %w", err)
	}
	if batch <= 0 {
		batch = 64
	}
	var texts, ids []string
	flush := func() error {
		if len(texts) == 0 {
			return nil
		}
		vecs, e := embed(ctx, texts)
		if e != nil {
			return e
		}
		for i, v := range vecs {
			if _, e := s.c.PatchDocument(ctx, s.sources, ids[i], map[string]any{
				"$set": map[string]any{"body_embed": v},
			}); e != nil {
				return e
			}
		}
		texts, ids = nil, nil
		return nil
	}
	res, e := s.c.Query(ctx, s.sources, client.Query{
		Filter: map[string]any{"status": source.StatusActive},
		Limit:  1000,
	})
	if e != nil {
		return 0, e
	}
	for _, d := range res.Documents {
		if _, ok := d["body_embed"]; ok {
			continue
		}
		id, _ := d["_id"].(string)
		body, _ := d["body"].(string)
		if id == "" || body == "" {
			continue
		}
		texts = append(texts, trimRunes(body, 2000))
		ids = append(ids, id)
		if len(texts) >= batch {
			if e := flush(); e != nil {
				return len(texts) - batch, e
			}
		}
	}
	if e := flush(); e != nil {
		return len(ids), e
	}
	// Report the true backfilled total (idempotent re-runs count everything).
	res, e = s.c.Query(ctx, s.sources, client.Query{Limit: 1000})
	if e != nil {
		return 0, e
	}
	n = 0
	for _, d := range res.Documents {
		if v, ok := d["body_embed"]; ok && v != nil {
			n++
		}
	}
	return n, nil
}

func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// JobDoc is the async-ingest state-machine state under KV ask:job:<name>
// (design §3.4.2: 进度必须真实可查).
type JobDoc struct {
	Job     string `json:"job"`
	State   string `json:"state"` // queued | running | done | failed
	Phase   string `json:"phase"` // extracting | normalizing | upserting
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Failed  int    `json:"failed"`
	Error   string `json:"error,omitempty"`
	Updated string `json:"updated"`
}

// PutJobDoc writes the job state (KV mirror of the resumable cursor).
func (s *Store) PutJobDoc(ctx context.Context, job string, d JobDoc) error {
	d.Job = job
	d.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return s.c.KVPut(ctx, s.jobs+job, raw, 0)
}

// GetJobDoc reads the job state; a missing job reports state "".
func (s *Store) GetJobDoc(ctx context.Context, job string) (JobDoc, error) {
	raw, err := s.c.KVGet(ctx, s.jobs+job)
	if err != nil || len(raw) == 0 {
		return JobDoc{Job: job}, nil
	}
	var d JobDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return JobDoc{}, err
	}
	if d.Job == "" {
		d.Job = job
	}
	return d, nil
}

// IngestFiles walks a directory and upserts .md/.txt/.html files as sources
// (Path A minus extraction workers). Resumable under the job key — same cursor
// semantics as IngestJSONL — with a live job doc per file.
func (s *Store) IngestFiles(ctx context.Context, dir string, recursive bool, jobKey string) (int, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && !recursive {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".md", ".txt", ".html", ".htm", ".docx", ".pdf":
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Strings(files)
	if jobKey == "" {
		jobKey = "files"
	}
	cursorKey := s.jobs + jobKey
	start := 0
	if raw, err := s.c.KVGet(ctx, cursorKey); err == nil && len(raw) > 0 {
		if n, err := strconv.Atoi(string(raw)); err == nil {
			start = n
		}
	}
	_ = s.PutJobDoc(ctx, jobKey, JobDoc{
		State: "running", Phase: "extracting", Total: len(files), Done: start,
	})
	done := 0
	for i := start; i < len(files); i++ {
		p := files[i]
		raw, err := os.ReadFile(p)
		if err != nil {
			_ = s.PutJobDoc(ctx, jobKey, JobDoc{
				State: "failed", Phase: "extracting", Total: len(files),
				Done: done, Failed: 1, Error: err.Error(),
			})
			return done, err
		}
		ext := strings.ToLower(filepath.Ext(p))
		typ := "md"
		text := string(raw)
		switch ext {
		case ".txt":
			typ = "txt"
		case ".html", ".htm":
			typ = "html"
			text = ExtractHTML(text)
		case ".docx":
			typ = "docx"
			docxText, derr := ExtractDOCX(raw)
			if derr != nil {
				_ = s.PutJobDoc(ctx, jobKey, JobDoc{
					State: "failed", Phase: "extracting", Total: len(files),
					Done: done, Failed: 1, Error: derr.Error(),
				})
				return done, fmt.Errorf("file %s: %w", p, derr)
			}
			text = docxText
		case ".pdf":
			typ = "pdf"
			text = ExtractPDF(raw)
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			rel = filepath.Base(p)
		}
		_ = s.PutJobDoc(ctx, jobKey, JobDoc{
			State: "running", Phase: "normalizing", Total: len(files), Done: done,
		})
		src := source.New(filepath.Base(p), typ, "file://"+p, filepath.ToSlash(rel), "zh", text, nil)
		_ = s.PutJobDoc(ctx, jobKey, JobDoc{
			State: "running", Phase: "upserting", Total: len(files), Done: done,
		})
		if _, err := s.Put(ctx, src); err != nil {
			_ = s.PutJobDoc(ctx, jobKey, JobDoc{
				State: "failed", Phase: "upserting", Total: len(files),
				Done: done, Failed: 1, Error: err.Error(),
			})
			return done, fmt.Errorf("file %s: %w", p, err)
		}
		done++
		if err := s.c.KVPut(ctx, cursorKey, []byte(strconv.Itoa(i+1)), 0); err != nil {
			return done, err
		}
	}
	if err := s.PutJobDoc(ctx, jobKey, JobDoc{
		State: "done", Phase: "upserting", Total: len(files), Done: done,
	}); err != nil {
		return done, err
	}
	return done, nil
}

// MapSpec is the declarative Path B mapping (ingest-jsonl --map): body/title/
// key are {{field}} templates — never raw JSON serialization — while the
// listed fields pass through to meta for filtering (design §3.4.2).
type MapSpec struct {
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Key   string   `json:"key"`
	Type  string   `json:"type"`
	Lang  string   `json:"lang"`
	Meta  []string `json:"meta"`
}

// RenderMap turns one JSONL record into a source via the template spec.
func RenderMap(spec MapSpec, rec map[string]any) (source.Source, error) {
	if strings.TrimSpace(spec.Body) == "" {
		return source.Source{}, fmt.Errorf("ingest map: body template is required")
	}
	body, err := renderTemplate(spec.Body, rec)
	if err != nil {
		return source.Source{}, err
	}
	title, err := renderTemplate(spec.Title, rec)
	if err != nil {
		return source.Source{}, err
	}
	key, err := renderTemplate(spec.Key, rec)
	if err != nil {
		return source.Source{}, err
	}
	typ := spec.Type
	if typ == "" {
		typ = "jsonl"
	}
	lang := spec.Lang
	if lang == "" {
		lang = "zh"
	}
	meta := map[string]any{}
	for _, f := range spec.Meta {
		if v, ok := rec[f]; ok {
			meta[f] = v
		}
	}
	return source.New(title, typ, "", key, lang, body, meta), nil
}

var fieldRe = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

func renderTemplate(tmpl string, rec map[string]any) (string, error) {
	var err error
	out := fieldRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		name := strings.TrimSpace(fieldRe.FindStringSubmatch(m)[1])
		v, ok := rec[name]
		if !ok {
			if err == nil {
				err = fmt.Errorf("ingest map: field %q missing", name)
			}
			return ""
		}
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	})
	return out, err
}

// invalidateEvidence retires every live evidence window of a source and
// returns how many were patched.
func (s *Store) invalidateEvidence(ctx context.Context, docID string) (int, error) {
	res, err := s.c.Query(ctx, s.evidence, client.Query{
		Filter: map[string]any{"doc_id": docID, "status": "live"},
		Limit:  1000,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range res.Documents {
		id, _ := d["_id"].(string)
		if id == "" {
			continue
		}
		if _, err := s.c.PatchDocument(ctx, s.evidence, id, map[string]any{"$set": map[string]any{"status": "stale"}}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Store) findByBusiness(ctx context.Context, businessKey, title string) (*source.Source, error) {
	filter := map[string]any{}
	if businessKey != "" {
		filter["business_key"] = businessKey
	} else if title != "" {
		filter["title"] = title
	} else {
		return nil, nil
	}
	res, err := s.c.Query(ctx, s.sources, client.Query{Filter: filter, Limit: 8})
	if err != nil {
		return nil, err
	}
	var best *source.Source
	for _, d := range res.Documents {
		src, err := fromDoc(d)
		if err != nil {
			return nil, err
		}
		if src.Status == source.StatusDeleted {
			continue
		}
		if best == nil {
			best = src
			continue
		}
		if src.Status == source.StatusActive && best.Status != source.StatusActive {
			best = src
		}
	}
	return best, nil
}

func (s *Store) getSource(ctx context.Context, id string) (*source.Source, error) {
	d, err := s.c.GetDocument(ctx, s.sources, id)
	if err != nil {
		if client.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return fromDoc(d)
}

func (s *Store) getEvidence(ctx context.Context, id string) (map[string]any, error) {
	d, err := s.c.GetDocument(ctx, s.evidence, id)
	if err != nil {
		if client.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func toDoc(src source.Source) map[string]any {
	return map[string]any{
		"_id":          src.ID,
		"body":         src.Body,
		"title":        src.Title,
		"source_type":  src.SourceType,
		"source_uri":   src.SourceURI,
		"digest":       src.Digest,
		"structure":    structureToAny(src.Structure),
		"meta":         src.Meta,
		"lang":         src.Lang,
		"version":      src.Version,
		"status":       src.Status,
		"ingested_at":  src.IngestedAt.Format(time.RFC3339Nano),
		"updated_at":   src.UpdatedAt.Format(time.RFC3339Nano),
		"business_key": src.BusinessKey,
	}
}

func structureToAny(spans []source.Span) []any {
	out := make([]any, 0, len(spans))
	for _, s := range spans {
		out = append(out, map[string]any{
			"kind": s.Kind, "label": s.Label, "start": s.Start, "end": s.End,
		})
	}
	return out
}

func fromDoc(d map[string]any) (*source.Source, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var src source.Source
	if err := json.Unmarshal(raw, &src); err != nil {
		return nil, fmt.Errorf("decoding source: %w", err)
	}
	if src.ID == "" {
		if id, ok := d["_id"].(string); ok {
			src.ID = id
		}
	}
	return &src, nil
}
