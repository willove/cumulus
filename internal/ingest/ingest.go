// Package ingest is the dual-path write side of the cumulus-cluster suite: content-addressed
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

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
)

// Result reports what a single put did.
type Result struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // created | updated | unchanged (created = no live revision existed)
	Version int    `json:"version"`
	Digest  string `json:"digest"`
	StaleID string `json:"stale_id,omitempty"`
}

// Store writes clus_sources and clus_evidence against the storage Port. Every identity it
// touches is passed in already scoped (composite "ns:coll" identity for a
// tenant, bare name for the default library); namespace only scopes the flat
// KV keys the Store owns (job cursors, reconcile cursor) via ns.KV.
type Store struct {
	c         cumulite.Port
	sources   string
	evidence  string
	clusters  string
	namespace string
	jobs      string
}

func New(c cumulite.Port, sources, evidence, clusters, namespace string) *Store {
	if sources == "" {
		sources = "clus_sources"
	}
	if evidence == "" {
		evidence = "clus_evidence"
	}
	if clusters == "" {
		clusters = "clus_clusters"
	}
	return &Store{
		c: c, sources: sources, evidence: evidence, clusters: clusters,
		namespace: namespace, jobs: ns.KV(namespace, "clus:job:"),
	}
}

// MaxSyncBodyBytes is the synchronous put cap: larger corpora must
// go through the Job path (ingest-files / serve jobs), never block a request.
const MaxSyncBodyBytes = 256 << 10

// Put upserts one source as a revision of its business identity. Same digest as
// the live revision → unchanged. A different digest → the next revision number,
// with every other live revision of that identity retired (stale) and its
// evidence invalidated.
//
// The retire step is what makes this self-healing: an update interrupted after
// the insert (the previous revision never marked stale) leaves two live
// revisions, and the next Put — even of identical bytes — converges them.
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

	revs, err := s.revisions(ctx, src.BusinessKey, src.Title)
	if err != nil {
		return Result{}, err
	}
	live := latestActive(revs)

	if live != nil && live.Digest == src.Digest {
		// Nothing new to store, but still converge: an earlier attempt may have
		// left another revision live.
		if _, err := s.retireLive(ctx, revs, live.ID, now); err != nil {
			return Result{}, err
		}
		return Result{ID: live.ID, Status: "unchanged", Version: live.Version, Digest: live.Digest}, nil
	}

	next := 1
	for i := range revs {
		if revs[i].Version >= next {
			next = revs[i].Version + 1
		}
	}
	status := "created" // no live revision existed (first write, or re-import after a delete)
	if live != nil {
		status = "updated"
		src.IngestedAt = live.IngestedAt
		if src.BusinessKey == "" {
			src.BusinessKey = live.BusinessKey
		}
	} else {
		src.IngestedAt = now
	}
	src.ID = source.RevisionID(src.BusinessKey, src.Title, src.Digest, next)
	src.Version = next
	src.UpdatedAt = now

	if _, err := s.c.Insert(ctx, s.sources, []map[string]any{toDoc(src)}); err != nil {
		// A concurrent writer stored this revision first: that write is the
		// state, so report it instead of failing the caller.
		if existing, gerr := s.getSource(ctx, src.ID); gerr == nil && existing != nil {
			return Result{ID: existing.ID, Status: "unchanged", Version: existing.Version, Digest: existing.Digest}, nil
		}
		return Result{}, err
	}
	retired, err := s.retireLive(ctx, revs, src.ID, now)
	if err != nil {
		return Result{}, fmt.Errorf("retiring previous revisions: %w", err)
	}
	stale := ""
	if len(retired) > 0 {
		stale = retired[0]
	}
	return Result{ID: src.ID, Status: status, Version: next, Digest: src.Digest, StaleID: stale}, nil
}

// revisions returns every stored revision under one business identity, any
// status. The identity is the business key when present, else the title (the
// long-standing fallback). A document with neither has no identity to revise:
// it dedupes by content instead, so the caller gets a nil slice.
func (s *Store) revisions(ctx context.Context, businessKey, title string) ([]source.Source, error) {
	filter := map[string]any{}
	if businessKey != "" {
		filter["business_key"] = businessKey
	} else if title != "" {
		filter["title"] = title
	} else {
		return nil, nil
	}
	// Paginate: a long-lived key accumulates one stale revision per update
	// until Reclaim runs, and a truncated page would hide the live revision
	// from the version arithmetic below.
	const page = 1000
	var out []source.Source
	for skip := 0; ; skip += page {
		res, err := s.c.Query(ctx, s.sources, contract.Query{Filter: filter, Skip: skip, Limit: page})
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
			return out, nil
		}
	}
}

// latestActive is the revision an identity currently resolves to: the highest
// version still live. Choosing by version — not by whatever the store pages
// first, and not by page size — is what keeps a long revision history from
// regressing the current revision.
func latestActive(revs []source.Source) *source.Source {
	var best *source.Source
	for i := range revs {
		r := &revs[i]
		if r.Status != source.StatusActive {
			continue
		}
		if best == nil || r.Version > best.Version {
			best = r
		}
	}
	return best
}

// retireLive marks every live revision of the identity stale except keepID and
// invalidates their evidence. It is the single writer of the "one live revision
// per identity" invariant, so it both enforces uniqueness on a fresh store and
// repairs a store that a failed update left with two.
func (s *Store) retireLive(ctx context.Context, revs []source.Source, keepID string, now time.Time) ([]string, error) {
	var retired []string
	for i := range revs {
		r := &revs[i]
		if r.Status != source.StatusActive || r.ID == keepID {
			continue
		}
		if _, err := s.c.PatchDocument(ctx, s.sources, r.ID, map[string]any{
			"$set": map[string]any{
				"status":     source.StatusStale,
				"updated_at": now.Format(time.RFC3339Nano),
			},
		}); err != nil {
			return retired, fmt.Errorf("marking stale %s: %w", r.ID, err)
		}
		if _, err := s.invalidateEvidence(ctx, r.ID); err != nil {
			return retired, err
		}
		retired = append(retired, r.ID)
	}
	return retired, nil
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
		res, err := s.c.Query(ctx, s.sources, contract.Query{
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

// EvidenceHit is one live evidence window (clus_evidence) — the "history
// success" input to the prior's history arm.
type EvidenceHit struct {
	SourceID string  `json:"source_id"`
	Score    float64 `json:"score"`
	Snippet  string  `json:"snippet"`
}

// SourcesByIDs returns the ACTIVE sources with the given ids (unknown or
// retired ids are skipped). The reuse path validates a warm prior against
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
		res, err := s.c.Query(ctx, s.sources, contract.Query{
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
	res, err := s.c.Query(ctx, s.evidence, contract.Query{
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
	cursorKey := s.jobs + jobKey + jobCursorSuffix
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
// is declared once (ensure/scenario), never per ingest. clus_sources records a
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

// ReconcileReport summarizes one downstream pass over the clus_sources
// changelog (下游轮询消费，游标持久化，at-least-once + _id 幂等).
type ReconcileReport struct {
	Scanned        int    `json:"scanned"`
	EvInvalidated  int    `json:"evidence_invalidated"`
	ClustersMarked int    `json:"clusters_marked"`
	Cursor         uint64 `json:"cursor"`
}

// Reconcile consumes clus_sources changes since the persisted cursor: sources
// retired out-of-band (stale/deleted written without going through Put) get
// their live evidence invalidated and clusters anchored on them marked 待复核.
// Every action is idempotent, so reprocessing a page is safe — and it is also
// the safe choice: on failure the cursor stays where the last fully consumed
// page ended, so a read or patch that failed is retried instead of being
// skipped forever.
func (s *Store) Reconcile(ctx context.Context) (ReconcileReport, error) {
	rep := ReconcileReport{}
	cursorKey := ns.KV(s.namespace, "clus:reconcile:"+s.sources)
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
			// The document is read only to tell a live write (which Put already
			// handled) from a retirement. The cleanup itself keys off the change
			// id, so a source purged physically — document gone, changelog
			// record left behind — is still cleaned up.
			src, err := s.getSource(ctx, ch.ID)
			if err != nil {
				return rep, err
			}
			if src != nil && src.Status == source.StatusActive {
				continue
			}
			n, err := s.invalidateEvidence(ctx, ch.ID)
			if err != nil {
				return rep, fmt.Errorf("invalidating evidence for %s: %w", ch.ID, err)
			}
			rep.EvInvalidated += n
			marked, err := s.markClustersStale(ctx, ch.ID)
			if err != nil {
				return rep, fmt.Errorf("marking clusters of %s: %w", ch.ID, err)
			}
			rep.ClustersMarked += marked
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
// (emerging) — the reconcile-side trigger for the re-validation. It reports
// both the count and the error: swallowing a query failure here used to be
// indistinguishable from "there were no clusters to mark".
func (s *Store) markClustersStale(ctx context.Context, docID string) (int, error) {
	res, err := s.c.Query(ctx, s.clusters, contract.Query{
		Filter: map[string]any{"source_id": docID},
		Limit:  1000,
	})
	if err != nil {
		return 0, err
	}
	marked := 0
	for _, d := range res.Documents {
		id, _ := d["_id"].(string)
		if id == "" || d["lifecycle"] == "emerging" {
			continue
		}
		if _, err := s.c.PatchDocument(ctx, s.clusters, id, map[string]any{
			"$set": map[string]any{"lifecycle": "emerging"},
		}); err != nil {
			return marked, err
		}
		marked++
	}
	return marked, nil
}

// Reclaim physically removes retired sources ("保留是决策不是副作用"):
// tombstoned (deleted) always; stale revisions only with includeStale.
func (s *Store) Reclaim(ctx context.Context, includeStale bool) (int, error) {
	statuses := []string{source.StatusDeleted}
	if includeStale {
		statuses = append(statuses, source.StatusStale)
	}
	res, err := s.c.Query(ctx, s.sources, contract.Query{
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
	res, err := s.c.Query(ctx, s.evidence, contract.Query{
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
	if err := s.c.CreateIndexRequest(ctx, s.sources, contract.IndexRequest{
		Name: "clus_body_embed", Field: "body_embed", Type: "vector",
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
	// Paginate: a corpus larger than one page used to be silently truncated
	// (operator asked for a full backfill, got the first 1000). Same failure
	// mode as the earlier ActiveSources truncation.
	const page = 1000
	for skip := 0; ; skip += page {
		res, qe := s.c.Query(ctx, s.sources, contract.Query{
			Filter: map[string]any{"status": source.StatusActive},
			Skip:   skip,
			Limit:  page,
		})
		if qe != nil {
			return 0, qe
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
				if fe := flush(); fe != nil {
					return len(texts) - batch, fe
				}
			}
		}
		if len(res.Documents) < page {
			break
		}
	}
	if e := flush(); e != nil {
		return len(ids), e
	}
	// Report the true backfilled total (idempotent re-runs count everything).
	n = 0
	for skip := 0; ; skip += page {
		res, qe := s.c.Query(ctx, s.sources, contract.Query{
			Filter: map[string]any{"status": source.StatusActive},
			Skip:   skip,
			Limit:  page,
		})
		if qe != nil {
			return 0, qe
		}
		for _, d := range res.Documents {
			if v, ok := d["body_embed"]; ok && v != nil {
				n++
			}
		}
		if len(res.Documents) < page {
			break
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

// jobCursorSuffix keeps the resumable cursor out of the job document's key.
// They used to share one key, so every job-state write clobbered the cursor (a
// finished job restarted from zero) and the cursor read parsed JSON as an
// integer, silently falling back to zero.
const jobCursorSuffix = ":cursor"

// JobDoc is the async-ingest state-machine state under KV clus:job:<name>.
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
// walkIngestable collects the pipeline's extractable files under dir (same
// extension set the scan face reports), sorted for a stable cursor.
func walkIngestable(dir string, recursive bool) ([]string, error) {
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
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func (s *Store) IngestFiles(ctx context.Context, dir string, recursive bool, jobKey string) (int, error) {
	files, err := walkIngestable(dir, recursive)
	if err != nil {
		return 0, err
	}
	return s.ingestFileList(ctx, dir, files, jobKey)
}

// IngestCandidates ingests an explicit file list (a trimmed scan report or a
// hand-written one) through the SAME state machine as IngestFiles: resumable
// cursor, phase reporting, digest-idempotent upserts. P9's discovery step
// hands its survivors here — the walk is replaced, the pipeline is not.
func (s *Store) IngestCandidates(ctx context.Context, paths []string, jobKey string) (int, error) {
	if len(paths) == 0 {
		return 0, fmt.Errorf("ingest: candidate list is empty")
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	return s.ingestFileList(ctx, "", sorted, jobKey)
}

// relKey is the source's business key: walk-relative when dir is the walk
// root, base name otherwise (candidate lists have no common root).
func relKey(dir, p string) string {
	if dir == "" {
		return filepath.Base(p)
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return filepath.Base(p)
	}
	return rel
}

// ingestFileList runs the job state machine over an explicit file list. dir
// is the walk root ("" for candidate lists): it only shapes the source key,
// which falls back to the base name.
func (s *Store) ingestFileList(ctx context.Context, dir string, files []string, jobKey string) (int, error) {
	if jobKey == "" {
		jobKey = "files"
	}
	cursorKey := s.jobs + jobKey + jobCursorSuffix
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
				Done: start + done, Failed: 1, Error: err.Error(),
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
					Done: start + done, Failed: 1, Error: derr.Error(),
				})
				return done, fmt.Errorf("file %s: %w", p, derr)
			}
			text = docxText
		case ".pdf":
			typ = "pdf"
			text = ExtractPDF(raw)
		}
		rel := relKey(dir, p)
		_ = s.PutJobDoc(ctx, jobKey, JobDoc{
			State: "running", Phase: "normalizing", Total: len(files), Done: start + done,
		})
		src := source.New(filepath.Base(p), typ, "file://"+p, filepath.ToSlash(rel), "zh", text, nil)
		_ = s.PutJobDoc(ctx, jobKey, JobDoc{
			State: "running", Phase: "upserting", Total: len(files), Done: start + done,
		})
		if _, err := s.Put(ctx, src); err != nil {
			_ = s.PutJobDoc(ctx, jobKey, JobDoc{
				State: "failed", Phase: "upserting", Total: len(files),
				Done: start + done, Failed: 1, Error: err.Error(),
			})
			return done, fmt.Errorf("file %s: %w", p, err)
		}
		done++
		if err := s.c.KVPut(ctx, cursorKey, []byte(strconv.Itoa(i+1)), 0); err != nil {
			return done, err
		}
	}
	if err := s.PutJobDoc(ctx, jobKey, JobDoc{
		State: "done", Phase: "upserting", Total: len(files), Done: start + done,
	}); err != nil {
		return done, err
	}
	return done, nil
}

// MapSpec is the declarative Path B mapping (ingest-jsonl --map): body/title/
// key are {{field}} templates — never raw JSON serialization — while the
// listed fields pass through to meta for filtering.
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
	res, err := s.c.Query(ctx, s.evidence, contract.Query{
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

func (s *Store) getSource(ctx context.Context, id string) (*source.Source, error) {
	d, err := s.c.GetDocument(ctx, s.sources, id)
	if err != nil {
		if contract.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return fromDoc(d)
}

func (s *Store) getEvidence(ctx context.Context, id string) (map[string]any, error) {
	d, err := s.c.GetDocument(ctx, s.evidence, id)
	if err != nil {
		if contract.IsNotFound(err) {
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
