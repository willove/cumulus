package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/auth"
	"github.com/willove/cumulus/internal/corpus"
	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/store"
)

// 两个租户共用一个实例：**语料、索引、凭证面全部分开**。
//
// 这条测试是"后续整合项目"能不能安全共存的底线：只分集合不够（索引共用等于
// 门锁上了窗户），只分索引也不够（摄入会写进别人的集合）。
func TestTenantsAreIsolated(t *testing.T) {
	st, err := store.Open("", true)
	if err != nil {
		t.Fatal(err)
	}
	s := NewWithStore(st, nil, 3, 160)
	kr, err := auth.ParseKeyringSpec("alpha=sk-a,beta=sk-b")
	if err != nil {
		t.Fatal(err)
	}
	s.Keys = kr

	// alpha 写一篇自己的文档，beta 写另一篇（走公开摄入面，realm 由凭证推导）
	post := func(path, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		return rec
	}
	alpha := post("/v1/ingest", `{"body":"alpha 私有：连接池最大连接数是 100。","source":"paste"}`, "sk-a")
	if alpha.Code != http.StatusOK {
		t.Fatalf("alpha ingest: %d %s", alpha.Code, alpha.Body.String())
	}
	beta := post("/v1/ingest", `{"body":"beta 私有：服务端口默认 8484。","source":"paste"}`, "sk-b")
	if beta.Code != http.StatusOK {
		t.Fatalf("beta ingest: %d %s", beta.Code, beta.Body.String())
	}

	// 集合物理分开：查错集合拿到的是"没有"，不是"别人的数据"
	aDocs, err := corpus.LoadRealm(t.Context(), st, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	bDocs, err := corpus.LoadRealm(t.Context(), st, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(aDocs) != 1 || !strings.Contains(aDocs[0].Body, "alpha") {
		t.Fatalf("alpha realm must hold only its own doc: %+v", aDocs)
	}
	if len(bDocs) != 1 || !strings.Contains(bDocs[0].Body, "beta") {
		t.Fatalf("beta realm must hold only its own doc: %+v", bDocs)
	}

	// 索引分开：alpha 的索引里**不该**出现 beta 的文档
	aIdx, err := s.IndexFor(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	bIdx, err := s.IndexFor(t.Context(), "beta")
	if err != nil {
		t.Fatal(err)
	}
	// 索引隔离的正确断言是**命中的文档身份**，不是命中数：top-k 截断会把零分
	// 文档也返回（真跑踩过：用"命中数=0"证明隔离，结果 beta 的单篇索引照样返回
	// 那篇零分文档——数字对、结论错）。
	ownIDs := func(ds []retrieval.Document) map[string]bool {
		m := map[string]bool{}
		for _, d := range ds {
			m[d.ID] = true
		}
		return m
	}
	aOwn, bOwn := ownIDs(aDocs), ownIDs(bDocs)
	for _, id := range aIdx.Rank("连接池 端口 默认", 9) {
		if !aOwn[id] {
			t.Fatalf("alpha index returned a foreign doc %s", id)
		}
	}
	for _, id := range bIdx.Rank("连接池 端口 默认", 9) {
		if !bOwn[id] {
			t.Fatalf("beta index returned a foreign doc %s", id)
		}
	}
	// 反证：alpha 的 id 不在 beta 的索引里（否则上面两条是同义反复）
	for _, id := range bIdx.Rank("连接池 端口 默认", 9) {
		if aOwn[id] {
			t.Fatalf("alpha doc %s leaked into beta index", id)
		}
	}

	// 凭证面：alpha 的 key 永远拿不到 beta 的文档（/v1/doc）
	get := func(path, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		return rec
	}
	if rec := get("/v1/doc/"+bDocs[0].ID, "sk-a"); rec.Code != http.StatusNotFound {
		t.Fatalf("alpha must not read beta's doc: %d", rec.Code)
	}
	if rec := get("/v1/doc/"+aDocs[0].ID, "sk-a"); rec.Code != http.StatusOK {
		t.Fatalf("alpha must read its own doc: %d", rec.Code)
	}
}

// 鉴权：缺凭证 401、错凭证 403、health 放行。
func TestGateRejectsWithoutCredential(t *testing.T) {
	s := NewWithStore(newFakeStore(), nil, 3, 160)
	kr, _ := auth.ParseKeyringSpec("alpha=sk-a")
	s.Keys = kr

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/qa", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		return rec
	}
	if rec := post(`{"question":"x"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing credential must be 401: %d", rec.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/qa", strings.NewReader(`{"question":"x"}`))
	r.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad credential must be 403: %d", rec.Code)
	}
	// 探活不鉴权（否则负载均衡会把实例判死）
	h := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, h)
	if rec2.Code != http.StatusOK {
		t.Fatalf("health must stay open: %d", rec2.Code)
	}
}

// 客户端自称 realm 无效：身份只来自凭证。
func TestClientCannotClaimRealm(t *testing.T) {
	st, _ := store.Open("", true)
	s := NewWithStore(st, nil, 3, 160)
	kr, _ := auth.ParseKeyringSpec("alpha=sk-a,beta=sk-b")
	s.Keys = kr
	if _, err := corpus.CollectionFor("beta"), error(nil); err != nil {
		t.Fatal(err)
	}
	// 请求体里塞 realm 字段 + 用 alpha 的 key → 落进 alpha 集合
	r := httptest.NewRequest(http.MethodPost, "/v1/ingest", strings.NewReader(`{"body":"alpha 的文档","source":"paste","realm":"beta"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sk-a")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body.String())
	}
	aDocs, _ := corpus.LoadRealm(t.Context(), st, "alpha")
	bDocs, _ := corpus.LoadRealm(t.Context(), st, "beta")
	if len(bDocs) != 0 {
		t.Fatalf("a claimed realm must not route the write: beta got %d docs", len(bDocs))
	}
	if len(aDocs) != 1 {
		t.Fatalf("write must land in the credential's realm: alpha has %d", len(aDocs))
	}
}
