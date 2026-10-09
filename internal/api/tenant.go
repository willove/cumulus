package api

// tenant.go：**realm 的解析与传递**（多租户隔离的接线处）。
//
// 三条纪律：
//  1. realm 由**凭证推导**（见 internal/auth），不信请求体里的自称；
//  2. 语料按 realm **物理分集合**、索引按 realm **物理分表**——只分一边等于
//     门锁上了窗户；
//  3. 没配凭证表时回落到 Server.Realm（单机/本地开发的老路径一字不变）。

import (
	"net/http"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/corpus"
	"github.com/willove/cumulus/internal/retrieval"
)

// realmOf 取请求的 realm（经凭证推导；未鉴权时回落到 Server.Realm，单机老路径不变）。
func (s *Server) realmOf(r *http.Request) string {
	if v, ok := r.Context().Value(realmCtxKey{}).(string); ok && v != "" {
		return v
	}
	// **没有中间件时的兜底**（/v1/health 走的就是这条路：它免认证）。
	// 有凭证就按凭证的 realm 报告，没有才用启动参数——否则 health 会对着一个
	// 调用者根本不用的 realm 报数字（验收脚本第一次跑就抓到：realm="" 但语料有 3 篇）。
	if s.Keys != nil && !s.Keys.Empty() {
		if realm, err := s.Keys.Authenticate(r); err == nil && realm != "" {
			return realm
		}
	}
	return s.Realm
}

// realmCtxKey 是 realm 在 request context 里的键（私有类型：外部塞不进来）。
type realmCtxKey struct{}

// WithRealm 把 realm 写进 context（供网关与中间件使用）。
func WithRealm(ctx gocontext.Context, realm string) gocontext.Context {
	return gocontext.WithValue(ctx, realmCtxKey{}, realm)
}

// RealmFor 从 **flow 的 context** 取 realm（flow 里每一步的 context 都带着它，
// 比再从 HTTP 请求解一遍可靠——那是同一次问答，不该有两个真相）。
func (s *Server) RealmFor(c *context.Context) string {
	if c == nil {
		return s.Realm
	}
	if r := string(c.Realm()); r != "" {
		return r
	}
	return s.Realm
}

// indexForFlow 是 IndexFor 的 flow 版（flow 的 *context.Context 不实现
// std context.Context，所以不能直接传——**这里不新建上下文**，而是把 realm 传进去）。
func (s *Server) indexForFlow(c *context.Context, realm string) (*retrieval.Index, error) {
	docs, err := corpus.LoadRealm(gocontext.Background(), s.Store, realm)
	if err != nil {
		return nil, err
	}
	idx := retrieval.Build(docs)
	s.mu.Lock()
	if s.indexes == nil {
		s.indexes = map[string]*retrieval.Index{}
	}
	s.indexes[realm] = idx
	s.mu.Unlock()
	return idx, nil
}

// EscalateIndexFor 是升级取数用的取索引入口（惰性、认 realm）。
func (s *Server) EscalateIndexFor(c *context.Context) (*retrieval.Index, error) {
	if s.Store == nil {
		return s.Index(), nil
	}
	realm := s.RealmFor(c)
	s.mu.RLock()
	idx, ok := s.indexes[realm]
	s.mu.RUnlock()
	if ok && idx != nil {
		return idx, nil
	}
	return s.indexForFlow(c, realm)
}

// InvalidateRealm 作废某 realm 的缓存索引（下次读时按需重建）。
//
// **为什么必须有它**：摄入面写的是**请求 realm** 的集合，而 Rebuild 重建的是
// 默认 realm 的索引——不主动作废的话，多租户模式下"刚摄入的文档在问答里查不到"
// （真跑踩过：alpha 摄入后问自己的私货，拒答，因为它的索引里没有这篇）。
// 索引是缓存，缓存的正确做法是**写后作废**，而不是指望谁记得重建。
func (s *Server) InvalidateRealm(realm string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.indexes, realm)
}
