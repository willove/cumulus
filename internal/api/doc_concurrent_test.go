package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	gocontext "context"
	"os"
	"strings"

	"github.com/willove/cumulus/internal/corpus"
	"github.com/willove/cumulus/internal/docgen"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/retrieval"
)

// lockedStore 是**并发安全**的假 store。
//
// 为什么必须加锁：原 fakeStore 用裸 map，并发写会直接 panic（concurrent map writes）。
// 那不测出产品的问题、只测出替身的问题——所以这里给一个**线程安全的**替身，
// 让失败只可能来自**产品代码**。
type lockedStore struct {
	mu   sync.Mutex
	docs map[string]corpusDocShape // key: coll + "/" + id
}

func newLockedStore() *lockedStore { return &lockedStore{docs: map[string]corpusDocShape{}} }

func (f *lockedStore) key(coll, id string) string { return coll + "/" + id }
func (f *lockedStore) EnsureCollection(gocontext.Context, string) error {
	return nil
}
func (f *lockedStore) PutStruct(_ gocontext.Context, coll, id string, v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := v.(corpus.Doc); ok {
		f.docs[f.key(coll, id)] = corpusDocShape{ID: d.ID, Body: d.Body, Coll: coll, Version: d.Version}
	}
	return nil
}
func (f *lockedStore) GetStruct(_ gocontext.Context, coll, id string, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.docs[f.key(coll, id)]
	if !ok {
		return os.ErrNotExist
	}
	if dst, ok := out.(*corpus.Doc); ok {
		*dst = corpus.Doc{ID: d.ID, Body: d.Body, Version: d.Version}
	}
	return nil
}
func (f *lockedStore) Delete(_ gocontext.Context, coll, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.docs, f.key(coll, id))
	return nil
}
func (f *lockedStore) ListIDs(_ gocontext.Context, coll string, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k, d := range f.docs {
		if d.Coll == coll {
			out = append(out, strings.TrimPrefix(k, coll+"/"))
		}
	}
	return out, nil
}

// newConcurrentDocServer 造一个"store 里真有一篇文档"的并发用服务。
//
// 注意：**语料必须进 store**（不是 s.index）——docgen 走 IndexFor(按 realm)，
// 只挂 s.index 的服务在 docgen 这条路上是 0 窗口（我第一版就这么写，8 个请求全 503）。
func newConcurrentDocServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	st := newLockedStore()
	body := "专利权期限为二十年，自申请日起计算。论据甲与论据乙的说明性文字。"
	id := "src000000001"
	if err := st.PutStruct(gocontext.Background(), corpus.CollectionFor("alpha"), id,
		corpus.Doc{ID: id, Body: body, Encoding: "utf-8"}); err != nil {
		t.Fatal(err)
	}
	s := NewWithStore(st, nil, 9, 400)
	s.DocGen = &docgen.Generator{Client: concurrentGenLLM{}}
	s.Keys = keyringOf("alpha=sk-a")
	_ = retrieval.Build(nil)
	return s, s.Handler()
}

func (f *lockedStore) PutValue(gocontext.Context, string, []byte) error { return nil }
func (f *lockedStore) GetValue(gocontext.Context, string) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (f *lockedStore) Health(gocontext.Context) error { return nil }
func (f *lockedStore) Query(_ gocontext.Context, _ string, _ map[string]any, _, _ int, _ any) (int, error) {
	return 0, nil
}

// concurrentGenLLM 是**可并发**的假模型（每次返回略有不同的正文，模拟真实波动）。
type concurrentGenLLM struct{}

func (concurrentGenLLM) Complete(_ gocontext.Context, req llm.Request) (llm.Response, error) {
	n := 0
	for i := 0; i+1 < len(req.Prompt); i++ {
		if req.Prompt[i] == '[' && req.Prompt[i+1] >= '0' && req.Prompt[i+1] <= '9' {
			n++
		}
	}
	body := fmt.Sprintf(`{"title":"并发测试文档","sections":[{"heading":"H","claims":[%s]}]}`,
		`{"text":"论断甲","source":1},{"text":"论断乙","source":1}`)
	return llm.Response{Text: body}, nil
}

// 同一主题被**并发**生成 N 次：版本必须逐次递增，且**库里只有一篇**。
//
// 这是典型的 read-modify-write 竞态（读旧版 → 算版本 → 写回）：两个请求都读到 v1，
// 都写 v2 → 一次更新被静默吞掉。共享实例上"两个人同时整理同一个主题"就会发生。
func TestConcurrentDocGenerationSameTopicNoLostUpdate(t *testing.T) {
	s, h := newConcurrentDocServer(t)
	st := s.Store.(*lockedStore)
	const N = 8
	var wg sync.WaitGroup
	versions := make([]int, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/docs",
				strings.NewReader(`{"topic":"专利权期限","store":true,"top_k":3}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Cumulus-Key", "sk-a")
			h.ServeHTTP(rec, req)
			var out GenerateDocResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Title == "" {
				t.Errorf("请求失败（code=%d body=%.140s）", rec.Code, rec.Body.String())
				return
			}
			versions[i] = out.Version
		}(i)
	}
	wg.Wait()

	// 版本必须是 1..N 的一个排列（**每一次更新都被记下**，没有丢）
	seen := map[int]bool{}
	for _, v := range versions {
		if v < 1 || v > N {
			t.Fatalf("版本号越界（丢更新或串号）: %v", versions)
		}
		if seen[v] {
			t.Fatalf("版本号重复（两个并发请求拿到了同一个号 → 丢更新）: %v", versions)
		}
		seen[v] = true
	}
	if len(seen) != N {
		t.Fatalf("应有 %d 个不同版本，实际 %d: %v", N, len(seen), versions)
	}
	// 库里同主题只能有一篇（覆盖写，不是堆）
	ids, _ := st.ListIDs(gocontext.Background(), corpus.CollectionFor("alpha"), 0)
	count := 0
	for _, id := range ids {
		if strings.HasPrefix(id, "gen-") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("同主题并发生成后应只有一篇文档，实际 %d 篇: %v", count, ids)
	}
}

// 不同主题并发生成：互不干扰，各自一篇。
func TestConcurrentDocGenerationDifferentTopics(t *testing.T) {
	s, h := newConcurrentDocServer(t)
	st := s.Store.(*lockedStore)
	const N = 6
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/docs",
				strings.NewReader(`{"topic":"专利权期限 主题`+strconv.Itoa(i)+`","store":true,"top_k":3}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Cumulus-Key", "sk-a")
			h.ServeHTTP(rec, req)
		}(i)
	}
	wg.Wait()

	ids, _ := st.ListIDs(gocontext.Background(), corpus.CollectionFor("alpha"), 0)
	gen := 0
	for _, id := range ids {
		if strings.HasPrefix(id, "gen-") {
			gen++
		}
	}
	if gen != N {
		t.Fatalf("N 个不同主题应生成 N 篇，实际 %d 篇（并发丢写）: %v", gen, ids)
	}
}

// 并发读写：一边生成文档一边问答，不许 panic、不许把索引搞坏。
func TestConcurrentReadWhileWriting(t *testing.T) {
	_, h := newConcurrentDocServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/docs",
				strings.NewReader(`{"topic":"专利权期限 读写`+strconv.Itoa(i)+`","store":true,"top_k":3}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Cumulus-Key", "sk-a")
			h.ServeHTTP(rec, req)
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/qa",
				strings.NewReader(`{"question":"专利权期限多久"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Cumulus-Key", "sk-a")
			h.ServeHTTP(rec, req)
		}()
	}
	wg.Wait() // 断言：不 panic 即可（panic 会让测试直接挂）
}
