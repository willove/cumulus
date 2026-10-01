// Package ingest is the dual-path write side of the cumulus-cluster suite: content-addressed
// upsert of source documents, explicit staleness on update, tombstone delete,
// and a resumable batch job. Index materialization (embeddings) is out of band
// — put never blocks on a model.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/adapt"
	"github.com/willove/cumulus/internal/charset"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
	"github.com/willove/cumulus/internal/storedoc"
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

	// ensureMu/ensured back EnsureOnce: the suite collections are declared
	// lazily on the first write and then remembered. See EnsureOnce.
	ensureMu sync.Mutex
	ensured  bool

	// EmbedProgress reports backfill progress to stderr. Opt-in, set by the
	// operator-facing face (CLI ensure -embed) only: eval/search faces share
	// their output stream with machine-parsed JSON, and a progress line on
	// the merged stream is a contract break (the -l1pre e2e gate caught
	// exactly that).
	EmbedProgress bool
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
	storedoc.DeclareShape(context.Background(), c, sources, source.Source{})
	storedoc.DeclareShape(context.Background(), c, evidence, evDoc{})
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
// Put is the synchronous write path. It declares the suite collections first
// (once, memoized) so a fresh store accepts the first put instead of failing
// with a raw "collection not found" — see EnsureOnce for why this is
// memoized rather than a bare Ensure call, and for the two precedents it
// follows. The Job path calls put() directly and declares for itself, so it
// neither double-declares nor is affected.
func (s *Store) Put(ctx context.Context, src source.Source) (Result, error) {
	if err := s.EnsureOnce(ctx); err != nil {
		return Result{}, err
	}
	return s.put(ctx, src, MaxSyncBodyBytes)
}

// put is Put with an explicit body cap; cap <= 0 means uncapped. The async job
// path passes 0: SSOT §3.4.2 puts bodies over the synchronous cap on the Job
// path precisely so they can be stored, so rejecting them there would make a
// large document permanently un-ingestable.
func (s *Store) put(ctx context.Context, src source.Source, capBytes int) (Result, error) {
	if src.Body == "" {
		return Result{}, fmt.Errorf("ingest: body is required")
	}
	if capBytes > 0 && len(src.Body) > capBytes {
		return Result{}, fmt.Errorf("ingest: body is %d bytes, over the %d synchronous cap — use ingest-files or the serve job path", len(src.Body), capBytes)
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

	doc, derr := sourceDoc(src)
	if derr != nil {
		return Result{}, derr
	}
	if _, err := s.c.Insert(ctx, s.sources, []map[string]any{doc}); err != nil {
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

// liveRev is the slim per-identity state the batch path needs. It deliberately
// holds no body: a million-key index costs memory, and the body is not needed
// to decide revision arithmetic.
type liveRev struct {
	id         string
	version    int
	digest     string
	status     string
	ingestedAt time.Time
	bizKey     string
}

// BatchIngester does bulk upserts against ONE pre-loaded revision index, so a
// whole job is O(n) instead of O(n²). The engine indexes vectors only, so a
// filtered query on business_key is a full collection scan; doing that per
// document (or per 512-document batch) still left a quadratic term — measured:
// 3k records 27s, 6.4k more 207s, 107k >600s. One scan per job fixes it.
//
// Use it for bulk paths only (ingest-adapt / ingest-files / ingest-jsonl). It is
// not safe to share across concurrent jobs: the in-memory index would go stale.
type BatchIngester struct {
	st   *Store
	live map[string]liveRev
}

// NewBatchIngester scans the sources collection once and returns an ingester
// whose index is the current live-revision state of every business identity.
func (s *Store) NewBatchIngester(ctx context.Context) (*BatchIngester, error) {
	live := map[string]liveRev{}
	const page = 1000
	for skip := 0; ; skip += page {
		// Metadata only: the live-revision index never reads bodies or
		// vectors, so they stay out of the scan — a field newly read below
		// must be added to the projection too.
		res, err := s.c.Query(ctx, s.sources, contract.Query{
			Limit: page, Skip: skip,
			Projection: []string{"_id", "business_key", "digest", "status", "version", "ingested_at"},
		})
		if err != nil {
			return nil, err
		}
		for _, d := range res.Documents {
			key, _ := d["business_key"].(string)
			if key == "" {
				continue
			}
			r := liveRev{
				id:     docStr(d, "_id"),
				digest: docStr(d, "digest"),
				status: docStr(d, "status"),
				bizKey: key,
			}
			if v, ok := d["version"].(float64); ok {
				r.version = int(v)
			}
			r.ingestedAt = docTime(d, "ingested_at")
			if prev, ok := live[key]; !ok || r.version > prev.version ||
				(r.version == prev.version && r.ingestedAt.After(prev.ingestedAt)) {
				live[key] = r
			}
		}
		if len(res.Documents) < page {
			return &BatchIngester{st: s, live: live}, nil
		}
	}
}

// PutBatch upserts many sources. Semantics match Put exactly: same digest →
// unchanged, changed digest → next revision with the previous live revision
// retired and its evidence invalidated. Returns how many were stored.
func (b *BatchIngester) PutBatch(ctx context.Context, srcs []source.Source) (int, error) {
	n := 0
	now := time.Now().UTC()
	for i := range srcs {
		src := srcs[i]
		if src.Body == "" {
			continue
		}
		src.Digest = source.Digest(src.Body)
		if src.Status == "" {
			src.Status = source.StatusActive
		}
		identity := src.BusinessKey
		if identity == "" {
			identity = src.Title
		}
		prev, hasPrev := b.live[identity]
		if hasPrev && prev.digest == src.Digest && prev.status == source.StatusActive {
			continue // unchanged
		}
		next := 1
		if hasPrev && prev.version >= next {
			next = prev.version + 1
		}
		if hasPrev && prev.status == source.StatusActive {
			src.IngestedAt = prev.ingestedAt
			if src.BusinessKey == "" {
				src.BusinessKey = prev.bizKey
			}
		} else {
			src.IngestedAt = now
		}
		src.ID = source.RevisionID(identity, src.Title, src.Digest, next)
		src.Version = next
		src.UpdatedAt = now
		bdoc, berr := sourceDoc(src)
		if berr != nil {
			return n, berr
		}
		if _, ierr := b.st.c.Insert(ctx, b.st.sources, []map[string]any{bdoc}); ierr != nil {
			// Another writer stored this revision first: that write is the
			// state, so skip rather than fail the whole batch.
			if existing, gerr := b.st.getSource(ctx, src.ID); gerr == nil && existing != nil {
				continue
			}
			return n, ierr
		}
		retired := ""
		if hasPrev && prev.status == source.StatusActive && prev.id != src.ID {
			if _, perr := b.st.c.PatchDocument(ctx, b.st.sources, prev.id, map[string]any{
				"$set": map[string]any{"status": source.StatusStale, "updated_at": now},
			}); perr != nil {
				// A swallowed retire leaves two live revisions for one identity
				// with no path that ever converges them (Reconcile skips
				// actives) — same rule as the single-doc Put's retireLive.
				return n, fmt.Errorf("retiring previous revision %s: %w", prev.id, perr)
			}
			retired = prev.id
		}
		if retired != "" {
			if _, ierr := b.st.invalidateEvidence(ctx, retired); ierr != nil {
				return n, fmt.Errorf("invalidating evidence of retired %s: %w", retired, ierr)
			}
		}
		b.live[identity] = liveRev{
			id: src.ID, version: next, digest: src.Digest,
			status: source.StatusActive, ingestedAt: src.IngestedAt, bizKey: src.BusinessKey,
		}
		n++
	}
	return n, nil
}

func docStr(d map[string]any, k string) string {
	s, _ := d[k].(string)
	return s
}

func docTime(d map[string]any, k string) time.Time {
	s, _ := d[k].(string)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
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
	edoc, derr := storedoc.Doc(evDoc{
		ID: id, DocID: docID, Start: start, End: end, Score: score,
		Reasoning: reasoning, Snippet: snippet, Status: "live",
		Created: time.Now().UTC(),
	})
	if derr != nil {
		return "", derr
	}
	if _, err := s.c.Insert(ctx, s.evidence, []map[string]any{edoc}); err != nil {
		if existing, gerr := s.getEvidence(ctx, id); gerr == nil && existing != nil {
			return id, nil
		}
		return "", err
	}
	return id, nil
}

// IngestJSONL upserts a batch with a resumable cursor under the job key.
// IngestJSONL stores a JSONL corpus under a resumable job cursor. It runs
// through BatchIngester (one revision index per job), NOT per-record Put:
// the engine has no business_key index, so each Put's revisions() query was
// a full collection scan and the job was O(n²) — measured on the sibling
// bulk path, 6.4k records added 207s, 107k did not finish in 10 minutes.
// The BatchIngester contract already named this path; the wiring was simply
// never migrated. Cursor granularity is now per chunk (512) instead of per
// record: a resumed job re-processes up to 511 records, which the digest
// check makes idempotent. Empty bodies are skipped (batch-path contract,
// same as ingest-adapt), where the per-record path used to fail the job.
func (s *Store) IngestJSONL(ctx context.Context, jobKey string, records []map[string]any, mapFn func(map[string]any) (source.Source, error)) (int, error) {
	if jobKey == "" {
		jobKey = "default"
	}
	fp := recordsFingerprint(records)
	cursorKey := s.jobs + jobKey + jobCursorSuffix
	start := loadJobCursor(ctx, s.c, cursorKey, fp, len(records))
	bi, berr := s.NewBatchIngester(ctx)
	if berr != nil {
		return 0, berr
	}
	const chunk = 512
	done := 0
	for i := start; i < len(records); i += chunk {
		end := i + chunk
		if end > len(records) {
			end = len(records)
		}
		batch := make([]source.Source, 0, end-i)
		for j := i; j < end; j++ {
			src, err := mapFn(records[j])
			if err != nil {
				return done, fmt.Errorf("record %d: %w", j, err)
			}
			batch = append(batch, src)
		}
		if _, err := bi.PutBatch(ctx, batch); err != nil {
			return done, fmt.Errorf("record %d batch: %w", i, err)
		}
		done += end - i
		if err := s.saveJobCursor(ctx, cursorKey, fp, end); err != nil {
			return done, err
		}
	}
	return done, nil
}

// EnsureOnce declares the suite collections at most once per Store and is
// what the synchronous write path uses. Two constraints pull in opposite
// directions here and this is the shape that satisfies both:
//
//   - The engine fail-closes writes to collections it has never seen, so
//     `cumulus-cluster put` against a fresh store otherwise dies on a raw
//     "collection not found" and the operator has to know to run `ensure` by
//     hand first. The serve face states the same rule and solves it with a
//     per-namespace memo (cmd/cumulus-cluster/serve.go nsEnsurer); the Job
//     path calls Ensure explicitly. The CLI put face was the one left out.
//   - D7 says collection shape is declared once, never per ingest, and
//     Ensure writes a changelog record — so it must not run per document.
//
// Hence: declare lazily on first use, then remember. Only success is
// remembered, so a transient failure (a cancelled context, a locked store)
// retries on the next call instead of poisoning the Store for its lifetime.
func (s *Store) EnsureOnce(ctx context.Context) error {
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()
	if s.ensured {
		return nil
	}
	if _, err := s.Ensure(ctx); err != nil {
		return err
	}
	s.ensured = true
	return nil
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
//
// Anchoring has two shapes and both must count (SSOT §3.4.3: "依赖其的簇
// lifecycle→emerging 待复核"):
//
//   - the cluster's own source_id (the answer it was built from);
//   - any embedded evidence window pointing at the retired document. A
//     cluster folded by tidy/merge keeps only the WINNER's source_id while
//     still citing the loser's evidence, so matching source_id alone left
//     folded clusters looking fresh forever.
func (s *Store) markClustersStale(ctx context.Context, docID string) (int, error) {
	anchored := map[string]bool{}
	// Shape 1: the cluster's own answer source.
	for skip := 0; ; skip += 1000 {
		res, err := s.c.Query(ctx, s.clusters, contract.Query{
			Filter: map[string]any{"source_id": docID},
			Limit:  1000, Skip: skip,
			Projection: []string{"_id"},
		})
		if err != nil {
			return 0, err
		}
		for _, d := range res.Documents {
			if id, _ := d["_id"].(string); id != "" {
				anchored[id] = true
			}
		}
		if len(res.Documents) < 1000 {
			break
		}
	}
	// Shape 2: any evidence window that cites the retired document. The whole
	// collection is scanned (clusters ≤10³ per the scale assumption) because
	// the filter would have to reach into an array field.
	for skip := 0; ; skip += 1000 {
		// _id survives the inclusion by default; the only other field read
		// here is the evidence array, so content and embeddings stay out of
		// the scan.
		res, err := s.c.Query(ctx, s.clusters, contract.Query{
			Limit: 1000, Skip: skip,
			Projection: map[string]any{"evidence": 1},
		})
		if err != nil {
			return 0, err
		}
		for _, d := range res.Documents {
			id, _ := d["_id"].(string)
			if id == "" || anchored[id] {
				continue
			}
			if evidenceCitesDoc(d, docID) {
				anchored[id] = true
			}
		}
		if len(res.Documents) < 1000 {
			break
		}
	}
	marked := 0
	for id := range anchored {
		d, err := s.c.GetDocument(ctx, s.clusters, id)
		if err != nil || d == nil {
			continue
		}
		if d["lifecycle"] == "emerging" {
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

// evidenceCitesDoc reports whether a cluster document's embedded evidence
// array holds a window whose source is docID.
func evidenceCitesDoc(clusterDoc map[string]any, docID string) bool {
	raw, ok := clusterDoc["evidence"].([]any)
	if !ok {
		return false
	}
	for _, e := range raw {
		sm, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if src, _ := sm["source"].(string); src == docID {
			return true
		}
	}
	return false
}

// Reclaim physically removes retired sources ("保留是决策不是副作用"):
// tombstoned (deleted) always; stale revisions only with includeStale.
func (s *Store) Reclaim(ctx context.Context, includeStale bool) (int, error) {
	statuses := []string{source.StatusDeleted}
	if includeStale {
		statuses = append(statuses, source.StatusStale)
	}
	res, err := s.c.Query(ctx, s.sources, contract.Query{
		Filter:     map[string]any{"status": map[string]any{"$in": statuses}},
		Limit:      1000,
		Projection: []string{"_id"},
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
		Filter:     map[string]any{"doc_id": docID},
		Limit:      1000,
		Projection: []string{"_id"},
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
	done, skipped := 0, 0
	nextReport := 512
	report := func(force bool) {
		if !s.EmbedProgress {
			return
		}
		if !force && done < nextReport {
			return
		}
		log.Printf("[embed] embedded=%d skipped=%d", done, skipped)
		nextReport = done + 512
	}
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
		report(false)
		return nil
	}
	// Paginate: a corpus larger than one page used to be silently truncated
	// (operator asked for a full backfill, got the first 1000). Same failure
	// mode as the earlier ActiveSources truncation.
	const page = 1000
	for skip := 0; ; skip += page {
		// _id is kept through an inclusion by default; body is the embedding
		// input and body_embed the skip check, so everything else stays out.
		res, qe := s.c.Query(ctx, s.sources, contract.Query{
			Filter:     map[string]any{"status": source.StatusActive},
			Skip:       skip,
			Limit:      page,
			Projection: map[string]any{"body": 1, "body_embed": 1},
		})
		if qe != nil {
			return 0, qe
		}
		for _, d := range res.Documents {
			if _, ok := d["body_embed"]; ok {
				skipped++
				continue
			}
			id, _ := d["_id"].(string)
			body, _ := d["body"].(string)
			if id == "" || body == "" {
				continue
			}
			done++
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
	report(true)
	// Report the true backfilled total (idempotent re-runs count everything).
	n = 0
	for skip := 0; ; skip += page {
		// The count only checks body_embed; reading bodies to count vectors
		// was the expensive way around.
		res, qe := s.c.Query(ctx, s.sources, contract.Query{
			Filter:     map[string]any{"status": source.StatusActive},
			Skip:       skip,
			Limit:      page,
			Projection: map[string]any{"body_embed": 1},
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

// A resume cursor is a bare index into an input list, so it is only valid
// against the exact list it was written for. The stored value therefore
// carries the list's fingerprint ("fp:index"); on a mismatch — a different
// directory reusing a static job key ("files"/"adapt"/"default"), an edited
// list, or the old bare-integer format — the run restarts from zero. Upserts
// are digest-idempotent, so re-processing is the safe side; the old behaviour
// silently skipped that many files (or a whole smaller corpus) and still
// reported State:"done".
func listFingerprint(items []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00", len(items))
	for _, p := range items {
		fmt.Fprintf(h, "%s\x00", p)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func recordsFingerprint(records []map[string]any) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00", len(records))
	for _, r := range records {
		if b, err := json.Marshal(r); err == nil {
			h.Write(b)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// loadJobCursor reads a fingerprinted cursor; max bounds the index against a
// list that shrank under the same fingerprint (KV corruption, not normal
// operation — a matching fingerprint implies the same list).
func loadJobCursor(ctx context.Context, c cumulite.Port, key, fp string, max int) int {
	raw, err := c.KVGet(ctx, key)
	if err != nil || len(raw) == 0 {
		return 0
	}
	got, idx, ok := strings.Cut(string(raw), ":")
	if !ok || got != fp {
		return 0
	}
	n, err := strconv.Atoi(idx)
	if err != nil || n < 0 || n > max {
		return 0
	}
	return n
}

func (s *Store) saveJobCursor(ctx context.Context, key, fp string, next int) error {
	return s.c.KVPut(ctx, key, []byte(fp+":"+strconv.Itoa(next)), 0)
}

// JobDoc is the async-ingest state-machine state under KV clus:job:<name>.
type JobDoc struct {
	Job    string `json:"job"`
	State  string `json:"state"` // queued | running | done | failed
	Phase  string `json:"phase"` // extracting | normalizing | upserting
	Total  int    `json:"total"`
	Done   int    `json:"done"`
	Failed int    `json:"failed"`
	// Skipped counts files the run deliberately did not store (unreadable,
	// unextractable, empty after extraction). The job CONTINUES past them:
	// one bad file must not strand the rest of a directory (and a resumed
	// run must not re-fail on it forever). SkipReasons makes the accounting
	// auditable, mirroring the scan face's `skipped` map (P9: 跳过文件不入库，
	// skipped 账可查).
	Skipped     int            `json:"skipped,omitempty"`
	SkipReasons map[string]int `json:"skip_reasons,omitempty"`
	// SkipErrors is the per-file reason a file was skipped. A count alone says
	// "one file failed" without saying why, which is exactly when the operator
	// needs it.
	SkipErrors map[string]string `json:"skip_errors,omitempty"`
	// Records is the DOCUMENT count carried by the files counted in Done.
	// Done/Total stay in FILES so a progress bar never lies about units.
	Records int    `json:"records,omitempty"`
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
	if err != nil {
		if contract.IsNotFound(err) {
			return JobDoc{Job: job}, nil
		}
		return JobDoc{}, err
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

// WalkIngestable supplies one stable file snapshot for queue totals and processing.
func WalkIngestable(dir string, recursive bool) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && (!recursive || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		// Same rule as the P9 scan face (ingest/scan.go): dotfiles and editor
		// backups are not corpus. The two walkers disagreed before, so the
		// scan preview showed a file the directory walk would never ingest.
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
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
	files, err := WalkIngestable(dir, recursive)
	if err != nil {
		return 0, err
	}
	return s.IngestFileList(ctx, dir, files, jobKey)
}

// IngestFileList retains the discovery root when deriving document identities.
func (s *Store) IngestFileList(ctx context.Context, root string, paths []string, jobKey string) (int, error) {
	return s.ingestFileList(ctx, root, paths, jobKey, false)
}

// IngestUploaded keeps source identities and URIs independent of temporary staging paths.
func (s *Store) IngestUploaded(ctx context.Context, root string, paths []string, jobKey string) (int, error) {
	return s.ingestFileList(ctx, root, paths, jobKey, true)
}

// IngestCandidates ingests an explicit file list (a trimmed scan report or a
// hand-written one) through the SAME state machine as IngestFiles: resumable
// cursor, phase reporting, digest-idempotent upserts. P9's discovery step
// hands its survivors here — the walk is replaced, the pipeline is not.
// IngestAdapted streams one or more heterogeneous corpus files through the
// adapt package (JSON / JSON-lines / CSV / text autodetect) into the same
// content-addressed source store. It reuses the job state machine and cursor so
// an interrupted multi-file run resumes instead of restarting.
//
// files is walked in order; dir/recursive expand it first. The emit path is
// streaming, so a 600 MB corpus costs O(1) memory per record.
// IngestAdapted is the adapt-shaped twin of ingestFileList — see the warning
// on that function: the two must be changed together.
func (s *Store) IngestAdapted(ctx context.Context, files []string, f adapt.Fields, jobKey string) (int, error) {
	if len(files) == 0 {
		return 0, fmt.Errorf("ingest: no files")
	}
	// Same rule as ingestFileList: declare before the job cursor reads
	// clus_sources below. It is also this function's cancellation gate — Ensure
	// propagates ctx.Err() through the engine, so a cancelled run returns the
	// cancellation cause and runAdaptJob can record terminal progress, rather
	// than reaching NewBatchIngester and reading a store it never declared.
	if err := s.EnsureOnce(ctx); err != nil {
		return 0, err
	}
	if jobKey == "" {
		jobKey = "adapt"
	}
	fp := listFingerprint(files)
	cursorKey := s.jobs + jobKey + jobCursorSuffix
	start := loadJobCursor(ctx, s.c, cursorKey, fp, len(files))
	skip := map[string]int{}
	skipErr := map[string]string{}
	doneFiles := 0 // files fully processed (the unit Total counts)
	records := 0   // documents stored (reported separately)
	// skipTotal is the Skipped headline: the SUM over reasons. It used to read
	// skip["file"] — a key nobody writes on this path (reasons here are
	// "unreadable"/"adapt_failed"), so the job reported Skipped=0 forever while
	// SkipReasons held the real counts.
	skipTotal := func() int {
		t := 0
		for _, v := range skip {
			t += v
		}
		return t
	}
	// One revision index for the whole job. The alternative — rebuilding it per
	// batch — still rescanned the collection every 512 records and kept a
	// quadratic term (107k records did not finish inside 10 minutes).
	bi, berr := s.NewBatchIngester(ctx)
	if berr != nil {
		return 0, berr
	}
	progress := func(phase string) {
		_ = s.PutJobDoc(ctx, jobKey, JobDoc{
			State: "running", Phase: phase, Total: len(files),
			// Done is FILES, matching Total — it used to be record counts
			// against a file total, so a 16k-record run reported
			// done=16384 total=3 and never looked finished.
			Done: start + doneFiles, Skipped: skipTotal(),
			SkipReasons: skip, SkipErrors: skipErr,
			// Records counts DOCUMENTS this run stored — start is a FILE index
			// from the resume cursor; adding it here mixed units and inflated
			// the count by the resumed offset on every continued job.
			Records: records,
		})
	}
	progress("extracting")
	for i := start; i < len(files); i++ {
		if err := ctx.Err(); err != nil {
			return records, err
		}
		path := files[i]
		fh, err := os.Open(path)
		if err != nil {
			// Unreadable file: skip it and keep going (same rule as the plain
			// file walk — one bad file must not strand a corpus).
			skip["unreadable"]++
			// Keyed by the path AS LISTED: base names collide across
			// directories (a/doc.md vs b/doc.md), and the loser's reason —
			// the thing the operator is here to read — silently disappears.
			skipErr[path] = err.Error()
			doneFiles++
			if cerr := s.saveJobCursor(ctx, cursorKey, fp, i+1); cerr != nil {
				return records, cerr
			}
			progress("extracting")
			continue
		}
		n, aerr := s.adaptOne(ctx, bi, path, f)
		fh.Close()
		var sf *storeFailure
		if aerr != nil && errors.As(aerr, &sf) {
			// Storage refused the write (or the ctx ended): the file is not
			// malformed, so it must NOT become an adapt_failed skip — the job
			// fails here, the cursor stays at this file, a re-run retries it.
			return records, aerr
		}
		if aerr != nil {
			// An unrecognized container or a malformed record is a skipped
			// file with an auditable reason — INCLUDING the reason itself. A
			// bare count says "one file failed" without saying why, which is
			// exactly when the operator needs it.
			skip["adapt_failed"]++
			skipErr[path] = aerr.Error()
		} else {
			records += n
		}
		doneFiles++
		if cerr := s.saveJobCursor(ctx, cursorKey, fp, i+1); cerr != nil {
			return records, cerr
		}
		progress("upserting")
	}
	if err := s.PutJobDoc(ctx, jobKey, JobDoc{
		State: "done", Phase: "upserting", Total: len(files),
		Done: start + doneFiles, Skipped: skipTotal(), SkipReasons: skip,
		SkipErrors: skipErr, Records: records,
	}); err != nil {
		return records, err
	}
	return records, nil
}

// storeFailure marks storage-level failures inside the adapt stream: the file
// is NOT malformed — the engine refused a write or the ctx ended — so the job
// must fail instead of recording an adapt_failed skip and reporting done over
// lost data.
type storeFailure struct{ err error }

func (e *storeFailure) Error() string { return e.err.Error() }
func (e *storeFailure) Unwrap() error { return e.err }

// adaptOne streams one open file into the store, returning how many records it
// stored. Bodyless or unreadable records are skipped; a store-level error fails
// the file (and therefore the job), matching the plain walk's rule.
func (s *Store) adaptOne(ctx context.Context, b *BatchIngester, path string, f adapt.Fields) (int, error) {
	// Bounded batches: memory stays flat while the revision index — built ONCE
	// per job by NewBatchIngester — makes each record O(1).
	const batchSize = 512
	var batch []source.Source
	n := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		put, err := b.PutBatch(ctx, batch)
		n += put
		if err != nil {
			return &storeFailure{err}
		}
		batch = batch[:0]
		return nil
	}
	// StreamFile dispatches parquet to its own reader; everything else is
	// streamed from the handle we were given.
	err := adapt.StreamFile(ctx, path, f, func(d adapt.Doc) error {
		if strings.TrimSpace(d.Body) == "" {
			return nil
		}
		if len(d.Body) > maxAdaptBodyBytes {
			// A single record over the synchronous cap would be rejected by
			// put(); trim it rather than lose the document entirely. The trim
			// is recorded in meta so the operator can see it happened.
			if d.Meta == nil {
				d.Meta = map[string]any{}
			}
			d.Meta["truncated_bytes"] = len(d.Body) - maxAdaptBodyBytes
			d.Body = trimRunes(d.Body, maxAdaptBodyBytes)
		}
		batch = append(batch, source.New(d.Title, "jsonl", "file://"+path, d.Key, detectLang(d.Body), d.Body, d.Meta))
		if len(batch) >= batchSize {
			return flush()
		}
		return nil
	})
	if ferr := flush(); err == nil {
		err = ferr
	}
	return n, err
}

// maxAdaptBodyBytes keeps one adapted record inside a sane L0 document. A
// corpus record is a paragraph, not a book; anything larger is trimmed.
const maxAdaptBodyBytes = 64 << 10

// detectLang is a cheap script guess so a mixed corpus carries the right lang.
func detectLang(s string) string {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return "zh"
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return "en"
		}
	}
	return "zh"
}

func (s *Store) IngestCandidates(ctx context.Context, paths []string, jobKey string) (int, error) {
	if len(paths) == 0 {
		return 0, fmt.Errorf("ingest: candidate list is empty")
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	// A candidate list has no single walk root, so keying by base name would
	// collapse a/readme.md and b/readme.md onto ONE business identity — the
	// second silently superseding the first as a new revision. Key relative to
	// the longest shared directory instead (which also makes scan→ingest
	// produce the same identities as ingesting that directory directly), and
	// fall back to the full path when the list shares no root.
	return s.IngestFileList(ctx, commonRoot(sorted), sorted, jobKey)
}

// commonRoot is the longest directory shared by every path, or "" when they
// share none (mixed relative/absolute, or siblings). Segment-wise, so a shared
// name prefix that is not a directory boundary cannot produce a wrong root.
func commonRoot(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	seg := func(p string) []string {
		return strings.Split(filepath.ToSlash(filepath.Dir(filepath.Clean(p))), "/")
	}
	root := seg(paths[0])
	for _, p := range paths[1:] {
		d := seg(p)
		n := len(root)
		if len(d) < n {
			n = len(d)
		}
		i := 0
		for i < n && root[i] == d[i] {
			i++
		}
		root = root[:i]
		if len(root) == 0 {
			return ""
		}
	}
	if len(root) == 0 {
		return ""
	}
	// A lone empty segment means the paths share only the leading separator
	// (/xab/a.md vs /x/c.md): their common ancestor is the filesystem root,
	// which is still a usable key base — better than falling back to full
	// paths for every absolute candidate list.
	if strings.Join(root, "/") == "" {
		return filepath.FromSlash("/")
	}
	return filepath.FromSlash(strings.Join(root, "/"))
}

// relKey is the source's business key: walk-relative when dir is the walk
// root, the cleaned full path otherwise (candidate lists with no shared root).
// It is never the base name alone — that collides across directories.
func relKey(dir, p string) string {
	clean := filepath.Clean(p)
	if dir != "" {
		if rel, err := filepath.Rel(dir, clean); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(clean)
}

// ingestFileList runs the job state machine over an explicit file list. dir
// is the walk root (or the candidates' common root): it only shapes the source
// key. A file that cannot be READ or yields no text is skipped with an
// auditable reason and the cursor advances past it — one unreadable or
// unextractable file must not strand the rest of the directory, and a resumed
// run must not re-fail on it forever (P9: skipped 账可查). Only a store-level
// failure (the engine refusing the upsert) fails the job.
// ingestFileList and IngestAdapted below are near-duplicates of the same state
// machine: cursor resume, skip ledger, phase reporting, batch upsert. They
// MUST be changed together. They drifted once already — a lazy-declaration fix
// was applied to the wrong one of the pair and left
// TestAdaptCancelledJobPreservesTerminalProgress nil-dereferencing until the
// mismatch was found. Collapsing them is a real refactor with a real diff;
// until then this line is the warning.
func (s *Store) ingestFileList(ctx context.Context, dir string, files []string, jobKey string, uploaded bool) (ingested int, runErr error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Declare this namespace's collections before ANY read of them. The engine
	// fail-closes on undeclared collections, and the job cursor is read from
	// clus_sources a few lines below — so a declaration placed further down,
	// where this one used to sit (right before NewBatchIngester, with the
	// comment "declare first"), never ran on a genuinely fresh store:
	// `cumulus-cluster ingest-files -dir <corpus>` against a new -data died
	// with a raw `collection "clus_sources": not found`. Memoized like the
	// synchronous path, so a multi-file job still declares exactly once.
	if err := s.EnsureOnce(ctx); err != nil {
		return 0, err
	}
	if jobKey == "" {
		jobKey = "files"
	}
	fp := listFingerprint(files)
	cursorKey := s.jobs + jobKey + jobCursorSuffix
	start := loadJobCursor(ctx, s.c, cursorKey, fp, len(files))
	skip := map[string]int{}
	skipErr := map[string]string{}
	skipped := 0
	phase := "extracting"
	fileLabel := func(p string) string {
		if uploaded {
			return relKey(dir, p)
		}
		return p
	}
	bump := func(p, reason, detail string) {
		skip[reason]++
		skipErr[fileLabel(p)] = detail
		skipped++
	}
	// A resumed file offset must not inflate this run's document count.
	snapshot := func(state string) JobDoc {
		return JobDoc{
			State: state, Phase: phase, Total: len(files),
			Done: start + ingested + skipped, Skipped: skipped, SkipReasons: skip,
			SkipErrors: skipErr, Records: ingested,
		}
	}
	// Persist the actual counters even when the ingestion context has expired.
	defer func() {
		if runErr != nil {
			fw, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			doc := snapshot("failed")
			doc.Error = runErr.Error()
			_ = s.PutJobDoc(fw, jobKey, doc)
		}
	}()
	progress := func(next string) {
		phase = next
		_ = s.PutJobDoc(ctx, jobKey, snapshot("running"))
	}
	advance := func(i int) error {
		return s.saveJobCursor(ctx, cursorKey, fp, i+1)
	}
	progress("extracting")
	for i := start; i < len(files); i++ {
		if err := ctx.Err(); err != nil {
			return ingested, err
		}
		p := files[i]
		progress("extracting")
		raw, err := os.ReadFile(p)
		if err != nil {
			bump(p, "unreadable", err.Error())
			if cerr := advance(i); cerr != nil {
				return ingested, cerr
			}
			progress("extracting")
			continue
		}
		ext := strings.ToLower(filepath.Ext(p))
		typ := "md"
		text := string(raw)
		// Charset gate. Measured on a 1,251-file Chinese web-novel corpus:
		// 5.4% native UTF-8, 87.0% GB18030, 7.6% undecodable. Without this,
		// the 87% is stored as mojibake (every CJK char becomes one RuneError,
		// so rune counts — and therefore block and citation offsets — are wrong
		// by a large factor) and the 7.6% is stored as a document riddled with
		// U+FFFD, which answers queries while looking intact.
		//
		// Decoding happens BEFORE format extraction: ExtractHTML and friends
		// parse text, and a GBK byte stream is not a document they can parse.
		cs, cerr := charset.Decode(raw)
		if cerr != nil {
			bump(p, "undecodable", cerr.Error())
			if aerr := advance(i); aerr != nil {
				return ingested, aerr
			}
			progress("extracting")
			continue
		}
		text = cs.Text
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
				bump(p, "extract_failed", derr.Error())
				if cerr := advance(i); cerr != nil {
					return ingested, cerr
				}
				progress("extracting")
				continue
			}
			text = docxText
		case ".pdf":
			typ = "pdf"
			text = ExtractPDF(raw)
		}
		text = source.Normalize(text)
		if text == "" {
			bump(p, "empty", "no text after extraction and normalization")
			if cerr := advance(i); cerr != nil {
				return ingested, cerr
			}
			progress("normalizing")
			continue
		}
		rel := relKey(dir, p)
		uri := "file://" + p
		if uploaded {
			uri = "upload://" + rel
		}
		progress("normalizing")
		// Provenance for the conversion. The suite's document Digest covers the
		// CONVERTED + normalized body, so without these the file on disk can
		// never be tied back to the text the engine holds. src_digest is the
		// file as it was BEFORE transcoding.
		meta := map[string]any{
			"charset":    string(cs.Tier),
			"src_bytes":  cs.SrcBytes,
			"src_digest": cs.SrcDigest,
		}
		if cs.Converted {
			meta["charset_converted"] = true
		}
		src := source.New(filepath.Base(p), typ, uri, rel, "zh", text, meta)
		progress("upserting")
		// Async extraction is not subject to the synchronous Put body cap.
		// Long-document splitting is opt-in (CLUS_INGEST_BLOCKS) and routes
		// through PutBlock, which falls through to the plain put when the
		// switch is off — so the default path is the same call as before.
		if blkCfg := EnvBlockConfig(); blkCfg.enabled() {
			if _, err := s.PutBlock(ctx, src, blkCfg); err != nil {
				return ingested, fmt.Errorf("file %s: %w", fileLabel(p), err)
			}
		} else if _, err := s.put(ctx, src, 0); err != nil {
			return ingested, fmt.Errorf("file %s: %w", fileLabel(p), err)
		}
		ingested++
		if err := advance(i); err != nil {
			return ingested, err
		}
	}
	phase = "upserting"
	if err := s.PutJobDoc(ctx, jobKey, snapshot("done")); err != nil {
		return ingested, err
	}
	return ingested, nil
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
// invalidateEvidence marks every live window of one document stale. Paginated:
// a document with >1000 live windows used to keep its tail marked live, and
// stale windows are exactly what the reuse path must refuse.
func (s *Store) invalidateEvidence(ctx context.Context, docID string) (int, error) {
	const page = 1000
	n := 0
	for skip := 0; ; skip += page {
		res, err := s.c.Query(ctx, s.evidence, contract.Query{
			Filter: map[string]any{"doc_id": docID, "status": "live"},
			Limit:  page, Skip: skip,
		})
		if err != nil {
			return n, err
		}
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
		if len(res.Documents) < page {
			return n, nil
		}
	}
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

// evDoc is the stored shape of one evidence window — the keys the inline
// table used to write, now a struct so the typed path and the shape audit
// speak one vocabulary.
type evDoc struct {
	ID        string    `json:"_id"`
	DocID     string    `json:"doc_id"`
	Start     int       `json:"start"`
	End       int       `json:"end"`
	Score     float64   `json:"score"`
	Reasoning string    `json:"reasoning"`
	Snippet   string    `json:"snippet"`
	Status    string    `json:"status"`
	Created   time.Time `json:"created"`
}

// sourceDoc derives the stored document from source.Source's json tags —
// the single source of truth. The hand-maintained table this replaces wrote
// business_key UNCONDITIONALLY while the struct tag declares omitempty, a
// small live example of the two-serializer drift the typed path ends (no
// reader depends on the empty key: revisions() only ever filters a non-empty
// business_key). structureToAny is gone too — Span's own tags emit the same
// keys.
func sourceDoc(src source.Source) (map[string]any, error) {
	return storedoc.Doc(src)
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
