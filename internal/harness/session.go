package harness

import (
	gocontext "context"
	"fmt"
	"strings"
	"time"

	"github.com/willove/cumulus/internal/store"
)

// SessionCollection 是事件流的集合名（每场会话 = 一个 id 下的 N 个事件文档）。
const SessionCollection = "harness_sessions"

// SessionManifestCollection 放每场会话的清单（最后写，见 SessionLog.Commit）。
const SessionManifestCollection = "harness_session_meta"

// SessionManifest 是**一场会话的清单**（先读它，才知道读哪些事件）。
//
// 为什么清单最后写：半程被 kill 的会话不能被当成"完整的"——清单在，事件才可信。
// 这与评测档案的纪律同源（见 evalfcore/archive.go）。
type SessionManifest struct {
	SessionID string `json:"session_id"`
	// Count 是**已提交**的事件数（不含清单自身）。消费者据此判断"流完了"。
	Count int `json:"count"`
	// LastSeq 是最后一个事件的序号；0 = 还没有事件。
	LastSeq int `json:"last_seq"`
	// Complete 标记会话正常收尾（有 done 或 error 帧）。中途断开的会话
	// 同样可回放——**断线恢复不等于丢历史**。
	Complete  bool   `json:"complete"`
	UpdatedAt string `json:"updated_at"`
	// ExpiresAt 是这份会话事件的到期时刻（零值 = 不过期）。
	//
	// 为什么必须有：落库的是**用户提问原文 + 引用原文 + 思考过程**，多租户共用一个
	// store 时"只增不减"既是成本问题也是数据问题（谁问了什么会一直留着）。
	// 保留期必须**默认存在**而不是靠人记得删。
	ExpiresAt string `json:"expires_at,omitempty"`
}

// SessionLog 把事件流**落库**，让它可回放、可断线恢复。
//
// 为什么需要它（真跑才暴露）：一次流式问答常在十几秒到几十秒之间，而用户在移动
// 网络上会断线、会切后台被浏览器挂起、会刷新页面。只发一次 live 流的端点在这个
// 现实里**丢东西**——不是丢答案（答案还能重问），是丢"过程"（进度轨迹、已经流出
// 的思考与正文），而那恰恰是用户唯一无法重建的部分。
//
// 三条纪律：
//  1. **seq 连续**：事件序号由 Emitter 分配，落库后一一对应；回放按 seq 排序，
//     客户端拿 cursor（"我已经看到第几帧"）就能续上，缺口可检测。
//  2. **清单最后写**：没清单 = 没提交，半程被杀不冒充完整。
//  3. **回放形状 = 实时形状**：回放吐的帧与 live 一模一样（同一构造函数），
//     所以客户端**一套解析器**通吃实时与回放——这是能不能少写一整套状态机的关键。
type SessionLog struct {
	port  store.Port
	id    string
	count int
	last  int
	ttl   time.Duration // 保留期；0 = 跟着清单里已有的 ExpiresAt，不改写
}

// OpenSession 打开（或续写）一场会话的日志。
func OpenSession(ctx gocontext.Context, port store.Port, sessionID string) (*SessionLog, error) {
	return OpenSessionTTL(ctx, port, sessionID, 0)
}

// DefaultSessionTTL 是默认保留期（7 天）。事件里装的是提问原文与思考过程，不是
// 该永久保存的东西。
const DefaultSessionTTL = 7 * 24 * time.Hour

// OpenSessionTTL 打开会话并设置保留期。ttl=0 → DefaultSessionTTL；
// 续写已有会话时**保留原有的到期时刻**（不因续写而无限续命）。
func OpenSessionTTL(ctx gocontext.Context, port store.Port, sessionID string, ttl time.Duration) (*SessionLog, error) {
	if port == nil {
		return nil, fmt.Errorf("harness: session log needs a store")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("harness: session id required")
	}
	if err := port.EnsureCollection(ctx, SessionCollection); err != nil {
		return nil, fmt.Errorf("harness: ensure collection: %w", err)
	}
	if err := port.EnsureCollection(ctx, SessionManifestCollection); err != nil {
		return nil, fmt.Errorf("harness: ensure manifest collection: %w", err)
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	log := &SessionLog{port: port, id: sessionID, ttl: ttl}
	// 续写：从清单恢复游标（同一 session 二次提问 = 接着写，不是重开）
	var man SessionManifest
	if err := port.GetStruct(ctx, SessionManifestCollection, sessionID, &man); err == nil {
		log.count, log.last = man.Count, man.LastSeq
	}
	return log, nil
}

// Record 落一帧（幂等：同 seq 重复写入是覆盖，不会出现两份）。
func (l *SessionLog) Record(ctx gocontext.Context, ev Event) error {
	if l == nil || l.port == nil {
		return nil // 没挂日志 = 这一层没接（合法状态）
	}
	if ev.Seq <= 0 {
		return fmt.Errorf("harness: session event must carry seq (got %d)", ev.Seq)
	}
	if err := l.port.PutStruct(ctx, SessionCollection, l.eventID(ev.Seq), ev); err != nil {
		return fmt.Errorf("harness: record event %d: %w", ev.Seq, err)
	}
	if ev.Seq > l.last {
		l.last = ev.Seq
	}
	// 计数按**实际写入过的最大序号**走，不靠调用次数——重复 Record 不该把清单
	// 数大（否则回放会多出空位，客户端以为有缺口）。
	if ev.Seq > l.count {
		l.count = ev.Seq
	}
	if ev.Kind == KindDone || ev.Kind == KindError {
		return l.Commit(ctx, true)
	}
	// **增量提交**：每 N 帧写一次清单。理由是真实场景逼出来的——一次问答几十秒，
	// 用户在**跑到一半**时掉线是常态；清单只在终态写的话，"进行中的会话"根本
	// 查不到（真跑踩过：客户端读了 4 帧就断，问清单得到 404，补页无从下手）。
	// 代价：清单会写多次，读到的 count 可能**略微落后**于实际——它本来就是进度
	// 读数，不是账本；补页按 seq 直接读事件，不依赖清单的 count。
	if ev.Seq%commitEvery == 0 {
		return l.Commit(ctx, false)
	}
	return nil
}

// commitEvery 是增量提交清单的间隔（帧）。
const commitEvery = 8

// Commit 写清单（最后一步）。complete=true 表示会话正常收尾。
func (l *SessionLog) Commit(ctx gocontext.Context, complete bool) error {
	if l == nil || l.port == nil {
		return nil
	}
	man := SessionManifest{SessionID: l.id, Count: l.count, LastSeq: l.last, Complete: complete, UpdatedAt: nowRFC3339()}
	// 续写不重置到期时刻：否则"一直问同一 session"会让事件永不过期。
	if prev, ok := manifestOf(ctx, l.port, l.id); ok && prev.ExpiresAt != "" {
		man.ExpiresAt = prev.ExpiresAt
	} else if l.ttl > 0 {
		man.ExpiresAt = time.Now().Add(l.ttl).UTC().Format(time.RFC3339Nano)
	}
	if err := l.port.PutStruct(ctx, SessionManifestCollection, l.id, man); err != nil {
		return fmt.Errorf("harness: commit session manifest: %w", err)
	}
	return nil
}

// Manifest 读清单（消费者据此判断"还剩多少""完了吗"）。
func (l *SessionLog) Manifest(ctx gocontext.Context) (SessionManifest, bool) {
	var man SessionManifest
	if l == nil || l.port == nil {
		return man, false
	}
	if err := l.port.GetStruct(ctx, SessionManifestCollection, l.id, &man); err != nil {
		return man, false
	}
	return man, true
}

// Replay 回放事件：只给 seq > cursor 的，按序。limit<=0 表示不限。
//
// **缺口语义**：cursor 之前的事件若已被清理（本实现不做清理，这里只是把契约写
// 清楚），客户端会收到 seq 不连续——它的 onGap 回调就是为这种情况准备的。
// 我们**不静默补齐**：伪造中间帧比重放一个缺口更坏。
func Replay(ctx gocontext.Context, port store.Port, sessionID string, cursor, limit int) ([]Event, SessionManifest, error) {
	if port == nil {
		return nil, SessionManifest{}, fmt.Errorf("harness: replay needs a store")
	}
	var man SessionManifest
	man, ok := manifestOf(ctx, port, sessionID)
	if !ok {
		return nil, man, fmt.Errorf("harness: session %q not found", sessionID)
	}
	out := make([]Event, 0, min(limit, man.Count))
	for seq := cursor + 1; seq <= man.LastSeq; seq++ {
		if limit > 0 && len(out) >= limit {
			break
		}
		var ev Event
		if err := port.GetStruct(ctx, SessionCollection, eventID(sessionID, seq), &ev); err != nil {
			return out, man, nil // 单帧读不到就停：已读到的先给，缺口由客户端发现
		}
		out = append(out, ev)
	}
	return out, man, nil
}

func manifestOf(ctx gocontext.Context, port store.Port, sessionID string) (SessionManifest, bool) {
	// nil store = 这个服务没挂持久化（demo/测试常用）。返回"没有清单"而不是
	// 崩在这里——**缺席是合法状态**，API 层据此回 404。
	if port == nil {
		return SessionManifest{}, false
	}
	var man SessionManifest
	if err := port.GetStruct(ctx, SessionManifestCollection, sessionID, &man); err != nil {
		return man, false
	}
	return man, true
}

// SessionManifestOf 是给 API 层的便捷读法（会话是否存在、到哪了）。
func SessionManifestOf(ctx gocontext.Context, port store.Port, sessionID string) (SessionManifest, bool) {
	return manifestOf(ctx, port, sessionID)
}

func (l *SessionLog) eventID(seq int) string { return eventID(l.id, seq) }

func eventID(sessionID string, seq int) string {
	return fmt.Sprintf("%s/%06d", sessionID, seq)
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// RecordingSink 是**边发边录**的出口：事件照常送给下游（live 消费），同时落库
// （供回放）。它是"同一份事件两处消费"的机械保证——回放的不是另一条链路，
// 就是这条流本身记下来的，所以两处**不可能不一致**。
type RecordingSink struct {
	Live    Sink // 可空：只录不发（比如离线跑评测时只要历史）
	Log     *SessionLog
	SinkErr error // 最近一次落库失败（录不进去必须查得到）
}

// NewRecording 组一个边发边录的出口。
func NewRecording(live Sink, log *SessionLog) *RecordingSink {
	return &RecordingSink{Live: live, Log: log}
}

// Write 发给 live（若接了）并落库。落库失败**不阻断**（契约 3：降级可见不阻断），
// 但会留在 SinkErr 里供读数查看。
func (r *RecordingSink) Write(ev Event) error {
	var liveErr error
	if r.Live != nil {
		liveErr = r.Live.Write(ev)
	}
	if r.Log != nil {
		if err := r.Log.Record(gocontext.Background(), ev); err != nil {
			r.SinkErr = err
		}
	}
	return liveErr
}

// Expired 判断这份清单是否已过期（解析失败按**未过期**处理：宁可多留，
// 也不因为一个坏时间戳把整场会话删掉——删除是不可逆的）。
func (m SessionManifest) Expired(now time.Time) bool {
	if m.ExpiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, m.ExpiresAt)
	if err != nil {
		return false
	}
	return now.After(t)
}

// PruneSession 删掉一场会话的事件与清单（用户显式删除 / 到期清理都走它）。
//
// 返回删掉的事件数。清单写在最后删：中途失败时清单还在，读到的仍是"可回放"，
// 不会出现"清单说有事件、实际没了"的空洞。
func PruneSession(ctx gocontext.Context, port store.Port, sessionID string) (int, error) {
	if port == nil {
		return 0, fmt.Errorf("harness: prune needs a store")
	}
	man, ok := manifestOf(ctx, port, sessionID)
	if !ok {
		return 0, nil // 没有就是没有（幂等）
	}
	n := 0
	for seq := 1; seq <= man.LastSeq; seq++ {
		if err := port.Delete(ctx, SessionCollection, eventID(sessionID, seq)); err != nil {
			return n, fmt.Errorf("harness: prune event %d: %w", seq, err)
		}
		n++
	}
	if err := port.Delete(ctx, SessionManifestCollection, sessionID); err != nil {
		return n, fmt.Errorf("harness: prune manifest: %w", err)
	}
	return n, nil
}

// PruneExpired 清理所有已过期的会话（返回清理了几场、几帧）。
//
// 这是**唯一**该跑在后台的删除路径：到期就删，不等谁来点。
func PruneExpired(ctx gocontext.Context, port store.Port, now time.Time) (sessions int, events int, err error) {
	if port == nil {
		return 0, 0, fmt.Errorf("harness: prune needs a store")
	}
	ids, err := port.ListIDs(ctx, SessionManifestCollection, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("harness: list manifests: %w", err)
	}
	for _, id := range ids {
		man, ok := manifestOf(ctx, port, id)
		if !ok || !man.Expired(now) {
			continue
		}
		n, derr := PruneSession(ctx, port, id)
		events += n
		if derr != nil {
			return sessions, events, derr
		}
		sessions++
	}
	return sessions, events, nil
}
