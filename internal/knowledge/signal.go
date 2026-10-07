package knowledge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 使用信号（"长"那一半的第一块地基）：把每一次真实问答变成可聚合的
// 信号，落本地盘。
//
// 信号族（两源）：
//   - 服务端推导（无前端改动）：session 里同一问句（归一化后精确匹
//     配）再次出现 = 再问。按上一轮答没答分成两种——拒答后又原样问
//     （refusal_unsatisfied：弃权没解决用户的问题）与答了又原样问
//     （answer_incomplete：答案没答全）。两者主人不同，不许混计数。
//   - 前端钩子：引用点击（cite）——用户点了答案里的哪条引用，是"这
//     条证据被真看了"的唯一诚实的近似。
//
// 隐私口径：信号只落本机文件，不出网；问句按原文记（单用户本机，
// 为了能聚合"哪类问题老被重问"）。换机器不带，删文件即消失。
const (
	SignalReaskAfterRefusal = "refusal_unsatisfied"
	SignalReaskAfterAnswer  = "answer_incomplete"
	SignalCitationClick     = "cite"
)

// Signal 一条使用信号。
type Signal struct {
	TS       time.Time `json:"ts"`
	Session  string    `json:"session,omitempty"`
	Kind     string    `json:"kind"`
	Question string    `json:"question,omitempty"` // 归一化问句（再问族有）
	Target   string    `json:"target,omitempty"`   // cite 的 "doc#span"
	Note     string    `json:"note,omitempty"`     // 人读备注
}

// SignalStore 信号库：append-only 信号 + 每会话最后一问（推导再问的
// 依据）。JSON 落盘，并发安全。
type SignalStore struct {
	path string

	mu       sync.Mutex
	sessions map[string]lastAsk // session → 最后一问（归一化）
	signals  []Signal           // 全部信号（按时间）
}

type lastAsk struct {
	Question string
	Refused  bool
}

// NewSignalStore 打开（或新建）信号库。空路径 = 内存态（不落盘）。
func NewSignalStore(path string) *SignalStore {
	st := &SignalStore{path: path, sessions: map[string]lastAsk{}}
	if path == "" {
		return st
	}
	if data, err := os.ReadFile(path); err == nil {
		var snap struct {
			Sessions map[string]lastAsk `json:"sessions"`
			Signals  []Signal           `json:"signals"`
		}
		if json.Unmarshal(data, &snap) == nil {
			if snap.Sessions != nil {
				st.sessions = snap.Sessions
			}
			st.signals = snap.Signals
		}
		// 读失败不阻塞：信号是资产不是依赖，坏文件从头记
	}
	return st
}

// Remember 记一次问答，返回**新产生的信号**（再问族；nil = 这次没触
// 发）。同一 session 同一归一化问句第二次出现才算——跨 session 重问
// 是不同用户路径的信号，不在此列（那是另一族，等真实使用看需要再
// 加，不预先建）。
func (st *SignalStore) Remember(session, question string, refused bool) *Signal {
	if session == "" || question == "" {
		return nil // 一次性问答（无 session）不记：没有"再问"的上下文
	}
	q := NormalizeQuestion(question)
	st.mu.Lock()
	defer st.mu.Unlock()
	prev, seen := st.sessions[session]
	st.sessions[session] = lastAsk{Question: q, Refused: refused}
	if !seen || prev.Question != q {
		return nil
	}
	kind := SignalReaskAfterAnswer
	note := "答过之后又原样问——答案没答全"
	if prev.Refused {
		kind = SignalReaskAfterRefusal
		note = "拒答之后又原样问——弃权没解决问题"
	}
	sig := Signal{TS: time.Now(), Session: session, Kind: kind, Question: q, Note: note}
	st.signals = append(st.signals, sig)
	st.persistLocked()
	return &sig
}

// Record 记一条外部信号（前端钩子，如引用点击）。
func (st *SignalStore) Record(sig Signal) {
	if sig.TS.IsZero() {
		sig.TS = time.Now()
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.signals = append(st.signals, sig)
	st.persistLocked()
}

// Counts 按类型的信号计数（聚合视图的第一层：族分布）。
func (st *SignalStore) Counts() map[string]int {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := map[string]int{}
	for _, s := range st.signals {
		out[s.Kind]++
	}
	return out
}

// TopQuestions 某族信号里最常被触发的问句（聚合的第二层：哪类问题
// 反复出问题——下一轮靶子从这张表里挑）。
func (st *SignalStore) TopQuestions(kind string, n int) []Signal {
	st.mu.Lock()
	defer st.mu.Unlock()
	byQ := map[string]int{}
	for _, s := range st.signals {
		if s.Kind == kind && s.Question != "" {
			byQ[s.Question]++
		}
	}
	all := make([]Signal, 0, len(byQ))
	for q, c := range byQ {
		all = append(all, Signal{Kind: kind, Question: q, Note: strconv.Itoa(c)})
	}
	// 计数降序（小集合，插排即可）
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && atoiNote(all[j].Note) > atoiNote(all[j-1].Note); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	if n > 0 && len(all) > n {
		all = all[:n]
	}
	return all
}

// Len 信号总数。
func (st *SignalStore) Len() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.signals)
}

func (st *SignalStore) persistLocked() {
	if st.path == "" {
		return
	}
	snap := struct {
		Sessions map[string]lastAsk `json:"sessions"`
		Signals  []Signal           `json:"signals"`
	}{Sessions: st.sessions, Signals: st.signals}
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	// 落盘失败只丢这一次的持久化，信号仍在内存里继续产——
	// 信号是资产不是依赖，磁盘故障不该把问答面拖死
	_ = os.MkdirAll(filepath.Dir(st.path), 0o755)
	tmp := st.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, st.path)
	}
}

// NormalizeQuestion 归一化问句：去首尾空白 + 小写（中文无大小写，英
// 文问句因此共享缓存/再问判定）。
func NormalizeQuestion(q string) string {
	return strings.ToLower(strings.TrimSpace(q))
}

func atoiNote(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// TopCitations 被点最多的引用（cite 族按 target 聚合）。
func (st *SignalStore) TopCitations(n int) []Signal {
	st.mu.Lock()
	defer st.mu.Unlock()
	byT := map[string]int{}
	for _, s := range st.signals {
		if s.Kind == SignalCitationClick && s.Target != "" {
			byT[s.Target]++
		}
	}
	all := make([]Signal, 0, len(byT))
	for t, c := range byT {
		all = append(all, Signal{Kind: SignalCitationClick, Target: t, Note: strconv.Itoa(c)})
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && atoiNote(all[j].Note) > atoiNote(all[j-1].Note); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	if n > 0 && len(all) > n {
		all = all[:n]
	}
	return all
}
