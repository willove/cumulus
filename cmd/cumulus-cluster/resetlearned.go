package main

// Learning-state reset: every test and verification run must start from a
// KNOWN state, or the numbers lie. Two failure modes this fixes, both seen in
// practice:
//
//  1. A cluster left by an earlier run masks the code path under test (a
//     "cold" query actually hit reuse and never exercised the new logic).
//  2. The affinity ledger accumulates across runs, so the control arm and the
//     experiment arm no longer share a starting point.
//
// The reset clears ONLY derived learning state — clusters, cites, evidence,
// affinity, graph edges, conflicts, the ask-sequence cursor, and chat
// sessions. The CORPUS (clus_sources) is input, not learning, and is never
// touched. Everything is namespace-scoped and counted before and after, so a
// reset either reports what it removed or refuses to pretend it did.

import (
	"context"
	"fmt"
	"sort"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/ns"
)

// LearnedCollections are the derived-state collections a reset clears, in
// dependency order (evidence before the clusters that cite it).
func LearnedCollections(namespace string) []string {
	return []string{
		ns.Coll(namespace, "clus_affinity"),
		ns.Coll(namespace, "clus_evidence"),
		ns.Coll(namespace, "clus_cites"),
		ns.Coll(namespace, "clus_clusters"),
		ns.Coll(namespace, "clus_weak_edges"),
		ns.Coll(namespace, "clus_conflicts"),
	}
}

// sessionKVPrefix is the KV prefix chat sessions live under (session.go).
func sessionKVPrefix(namespace string) string { return ns.KV(namespace, "sess:") }

// cursorKVKey is the ask-sequence cursor (kb.Cursor, searchapi wiring).
func cursorKVKey(namespace string) string { return ns.KV(namespace, "clus:lastcluster") }

// ResetReport is what a reset removed — the audit trail for "was the state
// really clean?".
type ResetReport struct {
	Namespace string         `json:"namespace"`
	DryRun    bool           `json:"dry_run"`
	Docs      map[string]int `json:"docs"`
	KVKeys    int            `json:"kv_keys"`
	Total     int            `json:"total"`
}

// ResetLearned clears the namespace's derived learning state. dryRun counts
// without deleting (the "what would this touch?" preflight). Sources are
// never in the list, so the corpus survives by construction, not by a flag.
// evidenceColl is the deployment's evidence collection (the serve face takes
// it as a flag, so the reset must clear the name the stack actually reads —
// clearing only the ns-prefixed variant left the live evidence untouched).
func ResetLearned(ctx context.Context, c cumulite.Port, namespace, evidenceColl string, dryRun bool) (*ResetReport, error) {
	rep := &ResetReport{Namespace: namespace, DryRun: dryRun, Docs: map[string]int{}}
	colls := LearnedCollections(namespace)
	if evidenceColl != "" && !containsStr(colls, evidenceColl) {
		colls = append(colls, evidenceColl)
	}
	for _, coll := range colls {
		n, err := clearCollection(ctx, c, coll, !dryRun)
		if err != nil {
			return nil, fmt.Errorf("reset %s: %w", coll, err)
		}
		rep.Docs[coll] = n
		rep.Total += n
	}
	// KV: sessions + the ask cursor. Keys are listed then deleted; a key that
	// vanished between list and delete is reported as gone, not an error.
	for _, prefix := range []string{sessionKVPrefix(namespace), cursorKVKey(namespace)} {
		keys, err := c.KVKeys(ctx, prefix, 10000)
		if err != nil {
			return nil, fmt.Errorf("list kv %s: %w", prefix, err)
		}
		if !dryRun {
			for _, k := range keys {
				if _, err := c.KVDelete(ctx, k); err != nil {
					return nil, fmt.Errorf("delete kv %s: %w", k, err)
				}
			}
		}
		rep.KVKeys += len(keys)
		rep.Total += len(keys)
	}
	return rep, nil
}

// clearCollection pages a collection. delete=false counts everything (the
// preflight); delete=true removes each page and re-queries, because deleting
// while paging shifts the stable order.
func clearCollection(ctx context.Context, c cumulite.Port, coll string, delete bool) (int, error) {
	const page = 500
	seen := 0
	for {
		q := contract.Query{Limit: page}
		if !delete {
			q.Skip = seen // counting pages forward; deleting re-reads from the top
		}
		res, err := c.Query(ctx, coll, q)
		if err != nil {
			if contract.IsNotFound(err) {
				return seen, nil // undeclared collection: nothing to clear
			}
			return seen, err
		}
		if len(res.Documents) == 0 {
			return seen, nil
		}
		for _, d := range res.Documents {
			id, _ := d["_id"].(string)
			if id == "" {
				id, _ = d["_key"].(string)
			}
			if id == "" {
				continue
			}
			if delete {
				if _, err := c.DeleteDocument(ctx, coll, id); err != nil && !contract.IsNotFound(err) {
					return seen, err
				}
			}
			seen++
		}
		if len(res.Documents) < page {
			return seen, nil // last page: counting done, deleting done
		}
		// full page: counting advances by Skip above; deleting re-queries the
		// (now smaller) top page on the next iteration
	}
}

// LearningReport counts the namespace's learning state — the "is it clean?"
// probe the bench and the HTTP face both use.
type LearningReport struct {
	Namespace string         `json:"namespace"`
	Docs      map[string]int `json:"docs"`
	KVKeys    int            `json:"kv_keys"`
	Total     int            `json:"total"`
	Clean     bool           `json:"clean"`
}

// LearningState counts derived state without touching it.
func LearningState(ctx context.Context, c cumulite.Port, namespace, evidenceColl string) (*LearningReport, error) {
	rep := &LearningReport{Namespace: namespace, Docs: map[string]int{}}
	colls := LearnedCollections(namespace)
	if evidenceColl != "" && !containsStr(colls, evidenceColl) {
		colls = append(colls, evidenceColl)
	}
	for _, coll := range colls {
		n, err := clearCollection(ctx, c, coll, false)
		if err != nil {
			return nil, err
		}
		rep.Docs[coll] = n
		rep.Total += n
	}
	for _, prefix := range []string{sessionKVPrefix(namespace), cursorKVKey(namespace)} {
		keys, err := c.KVKeys(ctx, prefix, 10000)
		if err != nil {
			return nil, err
		}
		rep.KVKeys += len(keys)
	}
	rep.Total += rep.KVKeys
	rep.Clean = rep.Total == 0
	return rep, nil
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// SortedCollNames keeps report output stable across runs.
func SortedCollNames(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
