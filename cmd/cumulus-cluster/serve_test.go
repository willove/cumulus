package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/bucket"
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

// M10 + bucket gate: a search must NAME a registered bucket. The gate refuses
// an empty one (no silent fallback to the default library) and an unregistered
// one, and a registered bucket in a namespace whose collections were never
// declared must still persist its cluster.
func TestSearchRequiresRegisteredBucket(t *testing.T) {
	ctx := context.Background()
	t.Setenv("AIGATE_BASE_URL", "") // offline stubs: no network in tests
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	const nsName = "searchonly"
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
	base := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := base.Ensure(ctx, suiteExtra("")...); err != nil {
		t.Fatal(err)
	}
	buckets := bucket.New(engine)
	mux := http.NewServeMux()
	ens := newNSEnsurer(engine, base, "", "clus_sources")
	registerSearchFace(mux, engine, base, "clus_sources", "", false, ens, buckets)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(body string) (*http.Response, map[string]any) {
		resp, err := http.Post(srv.URL+"/v1/search", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}

	// 1) No bucket named at all → refused, not defaulted.
	resp, out := post(`{"query":"连接池最大是多少"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unnamed bucket must be refused: %d %v", resp.StatusCode, out)
	}
	if _, ok := out["hint"]; !ok {
		t.Fatalf("the refusal must point at the bucket registry: %v", out)
	}
	// 2) Named but unregistered → refused.
	resp, _ = post(`{"query":"连接池最大是多少","ns":"` + nsName + `"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unregistered bucket must be refused: %d", resp.StatusCode)
	}
	// 3) Registered → serves, and persists the cluster into that namespace.
	if _, err := buckets.Create(ctx, nsName, "测试桶", ""); err != nil {
		t.Fatal(err)
	}
	resp, out = post(`{"query":"连接池最大是多少","ns":"` + nsName + `"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a registered bucket must serve: %d %v", resp.StatusCode, out)
	}
	if out["cluster_id"] == "" || out["cluster_id"] == nil {
		t.Fatalf("the answer must persist a cluster in the bucket: %+v", out)
	}
	// 4) The query is recorded against the bucket (registry telemetry).
	b, err := buckets.Get(ctx, nsName)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.Queries == 0 {
		t.Fatalf("the bucket must record the query: %+v", b)
	}
	// 5) Re-listing the bucket shows the corpus/cluster counters.
	if b.Sources <= 0 || b.Clusters <= 0 {
		t.Fatalf("bucket counters must be refreshed: %+v", b)
	}
}
