package api

import (
	gocontext "context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/willove/cumulus/internal/auth"
	"github.com/willove/cumulus/internal/corpus"
	"github.com/willove/cumulus/internal/knowledge/affinity"
	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/store"
)

func affinityServer(t *testing.T) (*Server, store.Port) {
	t.Helper()
	st, err := store.Open("", true)
	if err != nil {
		t.Fatal(err)
	}
	s := NewWithStore(st, nil, 3, 200)
	kr, err := auth.ParseKeyringSpec("alpha=sk-a,beta=sk-b")
	if err != nil {
		t.Fatal(err)
	}
	s.Keys = kr
	s.Synth = offlineQA() // 离线合成器（不引 LLM；这里测的是账本接线）
	return s, st
}

func askQA(t *testing.T, s *Server, question, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/qa", strings.NewReader(`{"question":"`+question+`"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// 问答之后，token×document 账本必须**真的有记录**——否则这套东西是死代码。
//
// 这条是移植回来的东西有没有接上线的唯一判据（原版是离线包，没有这条）。
func TestAnswerRecordsAffinityLedger(t *testing.T) {
	s, st := affinityServer(t)
	ctx := gocontext.Background()
	if err := st.EnsureCollection(ctx, corpus.CollectionFor("alpha")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []corpus.Doc{
		{ID: "d1", Body: "专利权期限为二十年，自申请日起计算。"},
		{ID: "d2", Body: "生活垃圾应当分类投放。"},
	} {
		if err := st.PutStruct(ctx, corpus.CollectionFor("alpha"), d.ID, d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.IndexFor(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	rec := askQA(t, s, "专利权期限是多少年", "sk-a")
	if rec.Code != 200 {
		t.Fatalf("qa status = %d body=%s", rec.Code, rec.Body.String())
	}
	toks := affinity.TrimTokens(retrieval.Fields("专利权期限是多少年"), 24)
	w, err := affinity.NewLedger(st, affinity.CollectionFor("alpha")).Weights(ctx, toks, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(w) == 0 {
		t.Fatalf("账本没有任何记录：问答后必须记下 token×document")
	}
}

// **按 realm 隔离**：beta 的问答不该写进 alpha 的账本（否则 A 租户的行为会
// 改写 B 租户的排序先验——那是跨租户泄漏）。
func TestAffinityLedgerIsolatedByRealm(t *testing.T) {
	s, st := affinityServer(t)
	ctx := gocontext.Background()
	for _, realm := range []string{"alpha", "beta"} {
		if err := st.EnsureCollection(ctx, corpus.CollectionFor(realm)); err != nil {
			t.Fatal(err)
		}
		if err := st.PutStruct(ctx, corpus.CollectionFor(realm), "d1",
			corpus.Doc{ID: "d1", Body: "专利权期限为二十年，自申请日起计算。"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.IndexFor(ctx, realm); err != nil {
			t.Fatal(err)
		}
	}
	if rec := askQA(t, s, "专利权期限是多少年", "sk-b"); rec.Code != 200 {
		t.Fatalf("beta qa = %d", rec.Code)
	}
	toks := affinity.TrimTokens(retrieval.Fields("专利权期限是多少年"), 24)
	w, _ := affinity.NewLedger(st, affinity.CollectionFor("alpha")).Weights(ctx, toks, time.Now())
	if len(w) != 0 {
		t.Fatalf("alpha 账本不该有 beta 的记录：%v", w)
	}
}

// **默认只记不排**：不显式开开关时，账本权重不得影响检索结果。
func TestLedgerIsRecordOnlyByDefault(t *testing.T) {
	if affinity.RerankEnabled() {
		t.Fatal("账本默认必须只记不排（原版实测：全局声望 −11pp，宁可不排）")
	}
	if w := (&Server{}).ledgerWeights(gocontext.Background(), "alpha", "专利权期限"); w != nil {
		t.Fatalf("未开重排时权重必须为空，实际 %v", w)
	}
	affinity.EnableRerank(true)
	defer affinity.EnableRerank(false)
	if !affinity.RerankEnabled() {
		t.Fatal("显式打开后应生效")
	}
}
