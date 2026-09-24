package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
)

// serve 的摄取面跑在「未声明集合即 fail-close」的引擎上：启动即声明默认域、
// 按请求 ns 首见懒声明（scopedStore 的组合）——否则工作台摄取面板在一个新库上
// 第一杯就以 raw "collection not found" 收场。进程内真引擎把这两步钉死。
func TestServeEnsuresNamespacesBeforeFirstWrite(t *testing.T) {
	t.Setenv("AIGATE_BASE_URL", "") // offline: no network in tests
	ctx := context.Background()

	engine, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer engine.Close()

	// Boot path: the default library's suite, fully declared.
	base := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := base.Ensure(ctx, suiteExtra("")...); err != nil {
		t.Fatalf("boot ensure (default library): %v", err)
	}

	// Scoped path: first sighting of a namespace declares its suite, then
	// accepts a write on the fresh store.
	scoped, scopedSources, err := storeForNS(engine, base, "", "t1", "clus_sources")
	if err != nil {
		t.Fatalf("storeForNS: %v", err)
	}
	if scopedSources != "t1:clus_sources" {
		t.Fatalf("scoped sources identity = %q", scopedSources)
	}
	if _, err := scoped.Ensure(ctx, suiteExtra("t1")...); err != nil {
		t.Fatalf("scoped ensure: %v", err)
	}
	res, err := scoped.Put(ctx, source.New("t1 手册", "md", "", "h1", "zh", "连接池上限 200", nil))
	if err != nil {
		t.Fatalf("scoped put after ensure: %v", err)
	}
	if res.Status != "created" {
		t.Fatalf("scoped put status = %+v", res)
	}

	// Isolation holds: the default library never sees the tenant's doc.
	if src, err := base.Get(ctx, res.ID); err != nil || src != nil {
		t.Fatalf("default library leaked tenant source: src=%v err=%v", src, err)
	}
}

// M10: a namespace that was only ever SEARCHED in must not die on the cluster
// persist. The REST search face used to skip the lazy per-namespace declaration
// the ingest/MCP faces do, so the first cluster write hit the engine's
// fail-closed "collection not found" and surfaced as a 500.
func TestSearchFacePersistsInUndeclaredNamespace(t *testing.T) {
	ctx := context.Background()
	t.Setenv("AIGATE_BASE_URL", "") // offline stubs: no network in tests
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	const nsName = "searchonly"
	// Corpus written CLI-side: ONLY clus_sources is declared for this tenant.
	// clus_clusters / clus_weak_edges / clus_cites are not — the exact state a
	// serve meets on a namespace it has only ever searched in.
	srcColl := ns.Coll(nsName, "clus_sources")
	if err := engine.EnsureCollection(ctx, srcColl); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Insert(ctx, srcColl, []map[string]any{{
		"_id": "src:d", "title": "手册", "source_type": "md",
		"body": "连接池最大 128，超时 30 秒。", "business_key": "d",
		"lang": "zh", "digest": "x", "status": "active", "version": 1,
	}}); err != nil {
		t.Fatal(err)
	}
	// The serve-level library IS declared (boot path).
	base := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := base.Ensure(ctx, suiteExtra("")...); err != nil {
		t.Fatal(err)
	}

	// Precondition: without declaration the cluster write really does fail.
	if _, err := engine.Insert(ctx, ns.Coll(nsName, "clus_clusters"),
		[]map[string]any{{"_id": "Cprobe", "topic_key": "t"}}); err == nil {
		t.Fatal("precondition: an undeclared collection must fail-closed")
	}

	mux := http.NewServeMux()
	ens := newNSEnsurer(engine, base, "", "clus_sources")
	registerSearchFace(mux, engine, base, "clus_sources", "", false, ens)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := `{"query":"连接池最大是多少","ns":"` + nsName + `"}`
	resp, err := http.Post(srv.URL+"/v1/search", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("search in an undeclared namespace must not fail: %d %s", resp.StatusCode, b)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["cluster_id"] == "" || out["cluster_id"] == nil {
		t.Fatalf("the answer must persist a cluster in the foreign ns: %+v", out)
	}
	// The ensurer is idempotent: a second search in the same ns re-declares
	// nothing and still succeeds.
	resp2, err := http.Post(srv.URL+"/v1/search", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second search must stay green: %d", resp2.StatusCode)
	}
}
