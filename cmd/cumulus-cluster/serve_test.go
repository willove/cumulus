package main

import (
	"context"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/ingest"
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
