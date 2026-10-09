// Package usage —— 每租户（realm）的**用量计量与配额**。
//
// 为什么需要：多租户只做隔离是不够的——共享实例上总有人会写爆它（一个脚本循环
// 打接口就能把语料检索和 LLM 预算烧光）。隔离管"谁看得见"，配额管"谁能用多少"。
//
// 四条纪律：
//
//  1. **计量先于配额**：先如实数（问题数、文档数、token），再谈限制。看不到用量
//     就无法讨论配额。
//  2. **token 不知道就说不知道**：`TokensKnown=false` 时不许填 0 冒充"没用"。
//     真实链路里 usage 常常缺（流式、某些 provider），编一个 0 就是在说谎。
//  3. **配额可解释**：超限时要说清"哪个 realm、超了哪个额度、什么时候重置"，
//     客户端才知道该等还是该找运维。
//  4. **进程内口径要说清**：本实现是**进程内**统计，重启归零。对个人知识库的
//     单进程服务够用（也是诚实的取舍），但**不能冒充"账务"**——真要多实例计费
//     得把计数落 store，那是另一件事。
package usage

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Kind 是被计量的动作。
type Kind string

const (
	KindQuestion Kind = "question" // 一次问答
	KindDoc      Kind = "doc"      // 一次知识文档生成（含更新）
)

// Counter 是一个 realm 的累计用量。
type Counter struct {
	Questions int64 `json:"questions"`
	Docs      int64 `json:"docs"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
	// TokensKnown 标记 token 计数是否可信。false = provider 没给 usage，
	// **不许当成 0**（那会让"花了钱"显示成"没用钱"）。
	TokensKnown bool   `json:"tokens_known"`
	FirstSeen   string `json:"first_seen,omitempty"`
	LastSeen    string `json:"last_seen,omitempty"`
}

// Quota 是限额（0 = 不限）。目前只有两个维度，够用且好解释。
type Quota struct {
	QuestionsPerDay int64 `json:"questions_per_day"`
	DocsPerDay      int64 `json:"docs_per_day"`
}

// Unbounded 是"不限"（无配额）。
func Unbounded() Quota { return Quota{} }

// Meter 是进程内的用量计量器（并发安全）。
type Meter struct {
	mu       sync.Mutex
	counters map[string]*Counter
	quota    map[string]Quota
	dayStart map[string]time.Time // 每个 realm 的当日计数起点
	now      func() time.Time     // 可注入（测试用）
}

// NewMeter 装一个计量器。quota 按 realm 配（没配的 realm = 不限）。
func NewMeter(quota map[string]Quota) *Meter {
	m := &Meter{now: time.Now}
	if quota == nil {
		quota = map[string]Quota{}
	}
	m.quota = map[string]Quota{}
	for realm, q := range quota {
		m.quota[realm] = q
	}
	m.resetLocked()
	return m
}

func (m *Meter) resetLocked() {
	m.counters = map[string]*Counter{}
	m.dayStart = map[string]time.Time{}
	now := m.now()
	for _, realm := range m.realmsLocked() {
		m.counters[realm] = &Counter{}
		m.dayStart[realm] = now
	}
}

func (m *Meter) realmsLocked() []string {
	out := make([]string, 0, len(m.quota))
	for r := range m.quota {
		out = append(out, r)
	}
	return out
}

// rolloverLocked 跨日就清零（配额是"每天"的）。
//
// **为什么自动清零而不是把用量累加**：配额语义是"今天最多用多少"，
// 跨日不清零就等于永久总量限制，那不是配额。
func (m *Meter) rolloverLocked(realm string) {
	now := m.now()
	start, ok := m.dayStart[realm]
	if !ok {
		m.dayStart[realm] = now
		m.counters[realm] = &Counter{}
		return
	}
	if now.Sub(start) >= 24*time.Hour {
		m.dayStart[realm] = now
		m.counters[realm] = &Counter{}
		return
	}
	if m.counters[realm] == nil {
		m.counters[realm] = &Counter{}
	}
}

// Exceeded 检查配额。返回 (超限, 可读原因)。
//
// **先检查再记账**：超限的那次不该被计数（否则一次拒绝会让剩余额度更快耗尽）。
func (m *Meter) Exceeded(realm string, kind Kind) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rolloverLocked(realm)
	q, has := m.quota[realm]
	if !has {
		return false, "" // 无配置 = 不限
	}
	c := m.counters[realm]
	var limit int64
	var what string
	switch kind {
	case KindQuestion:
		limit, what = q.QuestionsPerDay, "问答次数"
	case KindDoc:
		limit, what = q.DocsPerDay, "文档生成次数"
	}
	if limit <= 0 {
		return false, ""
	}
	var used int64
	switch kind {
	case KindQuestion:
		used = c.Questions
	case KindDoc:
		used = c.Docs
	}
	if used < limit {
		return false, ""
	}
	reset := m.dayStart[realm].Add(24 * time.Hour)
	return true, fmt.Sprintf("realm %s 今日%s已达上限 %d（用量 %d），配额于 %s 重置",
		realm, what, limit, used, reset.UTC().Format("15:04:05Z"))
}

// Record 记一次动作（超限的调用方不该调它——先 Exceeded 再 Record）。
func (m *Meter) Record(realm string, kind Kind) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rolloverLocked(realm)
	c := m.counters[realm]
	stamp := m.now().UTC().Format(time.RFC3339)
	switch kind {
	case KindQuestion:
		c.Questions++
	case KindDoc:
		c.Docs++
	}
	if c.FirstSeen == "" {
		c.FirstSeen = stamp
	}
	c.LastSeen = stamp
}

// RecordTokens 记 token（**不知道就不记**，并保持 TokensKnown=false）。
func (m *Meter) RecordTokens(realm string, in, out int64, known bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rolloverLocked(realm)
	c := m.counters[realm]
	if !known {
		return // provider 没给 usage：宁可"不知道"，不许填 0
	}
	c.TokensIn += in
	c.TokensOut += out
	c.TokensKnown = true
}

// Snapshot 是一个 realm 的用量读数（含配额与用量占比）。
type Snapshot struct {
	Realm    string  `json:"realm"`
	Usage    Counter `json:"usage"`
	Quota    Quota   `json:"quota"`
	ResetsAt string  `json:"resets_at,omitempty"`
	// Over flags 哪些维度已超限（读数自解释：不用去比数字）。
	Over map[string]bool `json:"over,omitempty"`
}

// Snapshot 取读数。
func (m *Meter) Snapshot(realm string) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rolloverLocked(realm)
	c := m.counters[realm]
	usage := *c
	if usage.TokensKnown {
		usage.TokensIn, usage.TokensOut = c.TokensIn, c.TokensOut
	}
	out := Snapshot{Realm: realm, Usage: usage, Quota: m.quota[realm]}
	if start, ok := m.dayStart[realm]; ok {
		out.ResetsAt = start.Add(24 * time.Hour).UTC().Format(time.RFC3339)
	}
	q := m.quota[realm]
	if q.QuestionsPerDay > 0 && usage.Questions >= q.QuestionsPerDay {
		if out.Over == nil {
			out.Over = map[string]bool{}
		}
		out.Over["questions"] = true
	}
	if q.DocsPerDay > 0 && usage.Docs >= q.DocsPerDay {
		if out.Over == nil {
			out.Over = map[string]bool{}
		}
		out.Over["docs"] = true
	}
	return out
}

// All 返回全部 realm 的读数（按 realm 排序，便于稳定输出）。
func (m *Meter) All() []Snapshot {
	m.mu.Lock()
	realms := map[string]bool{}
	for r := range m.counters {
		realms[r] = true
	}
	for r := range m.quota {
		realms[r] = true
	}
	m.mu.Unlock()

	out := make([]Snapshot, 0, len(realms))
	for r := range realms {
		out = append(out, m.Snapshot(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Realm < out[j].Realm })
	return out
}
