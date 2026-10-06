// Package knowledge 的会话复用件。
//
// 背景（evolution-log 三·补七的实测结论）：把候选区后验做成全局文档
// 声望，在真实语料上 −11pp（51 丢 / 18 赚）——一篇文章回答很多问题、
// 竞争文档各不相同，全局声望会把常被错引的热门文档沉底。 cumulus 的
// belief-update-design 本尊是**按查询/按会话**的：一次提问一份经验，
// 不跨问题积累。
//
// ReuseStore 是这个正确形态的第一版：按会话记录"这个问题问过，窗口
// 是这些， yield 如何"，同一问题（归一化后精确匹配）再问时直接取用。
// 它治的是"重复问"，不是"每个问题都变好"——后者要靠按查询候选区
// 后验（等 DEEP 多轮循环落地后接）。
package knowledge

import (
	"strings"
	"sync"
)

// Window 是复用的最小单元：引用坐标 + 原文（与 qaflow.EvidenceWindow
// 同字段；知识层不依赖流程层，两边在 qaflow 的适配处转换）。
type Window struct {
	SourceID string
	Span     string
	Text     string
	Score    float64
}

// Entry 是一个问题的复用记录。
type Entry struct {
	Question string
	Windows  []Window
	YieldHit bool    // 上轮这个问题是否命中金标（调用方给的反馈）
	Coverage float64 // 上轮的查询词覆盖度（同问同窗的属性，回放即诚实——
	// 不回放会让路由信号缺省成 0，把复用的问句误判成"没覆盖"而升级）
}

// ReuseStore 按会话 + 归一化问题存复用记录。并发安全。
type ReuseStore struct {
	mu     sync.Mutex
	bySess map[string]map[string]Entry
}

// NewReuseStore 建一个空 store（按 realm/会话注入）。
func NewReuseStore() *ReuseStore {
	return &ReuseStore{bySess: map[string]map[string]Entry{}}
}

// Normalize 归一化问题：去空白、小写。只做最保守的归一——同问题不同
// 标点也算不同问题（宁可漏命中，不可错复用：错复用是把错答案当经验）。
func Normalize(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(q))), " ")
}

// Lookup 找会话内同一问题的上轮记录。
func (s *ReuseStore) Lookup(session, question string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.bySess[session]
	if !ok {
		return Entry{}, false
	}
	e, ok := m[Normalize(question)]
	return e, ok
}

// Record 写入/覆盖一个问题的复用记录（同问题再问即刷新）。
func (s *ReuseStore) Record(session, question string, windows []Window, yieldHit bool, coverage float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.bySess[session]
	if !ok {
		m = map[string]Entry{}
		s.bySess[session] = m
	}
	m[Normalize(question)] = Entry{Question: question, Windows: windows, YieldHit: yieldHit, Coverage: coverage}
}

// Len 返回某会话的复用条数（观测/测试用）。
func (s *ReuseStore) Len(session string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bySess[session])
}
