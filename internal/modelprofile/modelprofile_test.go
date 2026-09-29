package modelprofile

import (
	"context"
	"testing"

	"github.com/willove/cumulite"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return New(engine)
}

func TestSaveGetListRoundtrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, Profile{ID: "mm", Label: "MiniMax", BaseURL: "https://api.example.com/v1", ChatModel: "M3", APIKey: "sk-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, Profile{ID: "local", Label: "本地", BaseURL: "http://127.0.0.1:9/v1"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d profiles, want 2", len(list))
	}
	// 空白 key 的更新保留已存 key（HTTP 面不回显，只能这样往返）。
	if _, err := s.Save(ctx, Profile{ID: "mm", Label: "MiniMax 改", BaseURL: "https://api2.example.com/v1", ChatModel: "M4"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "mm")
	if err != nil || got == nil {
		t.Fatalf("get after update: %v %v", got, err)
	}
	if got.APIKey != "sk-1" {
		t.Fatalf("empty-key update must keep the stored key, got %q", got.APIKey)
	}
	if got.ChatModel != "M4" || got.Label != "MiniMax 改" {
		t.Fatalf("update did not apply: %+v", got)
	}
}

func TestValidationAndActiveRules(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, Profile{ID: "bad id", BaseURL: "https://a.com"}); err == nil {
		t.Fatal("id with a space must be refused")
	}
	if _, err := s.Save(ctx, Profile{ID: "nokey-url"}); err == nil {
		t.Fatal("missing base_url must be refused")
	}
	if _, err := s.Save(ctx, Profile{ID: "p1", BaseURL: "https://a.com/v1"}); err != nil {
		t.Fatal(err)
	}
	// 激活的 profile 不可删除：先激活别的。
	if err := s.SetActive(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "p1"); err == nil {
		t.Fatal("removing the active profile must be refused")
	}
	if _, err := s.Save(ctx, Profile{ID: "p2", BaseURL: "https://b.com/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive(ctx, "p2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "p1"); err != nil {
		t.Fatalf("removing an inactive profile: %v", err)
	}
	active, err := s.ActiveID(ctx)
	if err != nil || active != "p2" {
		t.Fatalf("active = %q (%v), want p2", active, err)
	}
	// LastUsedAt 在激活时落戳。
	got, _ := s.Get(ctx, "p2")
	if got.LastUsedAt.IsZero() {
		t.Fatal("activation must stamp LastUsedAt")
	}
}
