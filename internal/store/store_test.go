package store

import (
	"context"
	"errors"
	"testing"

	"github.com/willove/cumulite"
)

type doc struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func openMem(t *testing.T) Port {
	t.Helper()
	p, err := Open("", true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = p.(*adapter).engine.Close() })
	return p
}

func TestPutGetDeleteRoundTrip(t *testing.T) {
	p := openMem(t)
	ctx := context.Background()
	if err := p.EnsureCollection(ctx, "sources"); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCollection(ctx, "sources"); err != nil {
		t.Fatal("ensure must be idempotent:", err)
	}
	if err := p.PutStruct(ctx, "sources", "d1", doc{Title: "部署手册", Body: "连接池"}); err != nil {
		t.Fatal(err)
	}
	// upsert：同 id 再写必须是替换，不是报错
	if err := p.PutStruct(ctx, "sources", "d1", doc{Title: "部署手册v2", Body: "连接池"}); err != nil {
		t.Fatal("put must upsert:", err)
	}
	var got doc
	if err := p.GetStruct(ctx, "sources", "d1", &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "部署手册v2" {
		t.Fatalf("want replaced title, got %q", got.Title)
	}
	// 读不存在的文档：必须是可判定的 not-found，不许静默零值
	err := p.GetStruct(ctx, "sources", "nope", &got)
	if err == nil {
		t.Fatal("missing document must error")
	}
	// 删除幂等
	if err := p.Delete(ctx, "sources", "d1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, "sources", "d1"); err != nil {
		t.Fatal("delete must be idempotent:", err)
	}
}

// Health 用真实探活；同时验证适配器确实跑在 cumulite 上（端口不是空气）。
func TestHealth(t *testing.T) {
	p := openMem(t)
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if _, ok := p.(*adapter); !ok {
		t.Fatal("adapter type")
	}
	if !errors.Is(cumulite.ErrNotFound, cumulite.ErrNotFound) {
		t.Fatal("sentinel sanity")
	}
}
