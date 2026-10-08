package harness

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

// Sink 是事件出口。实现方可以是 SSE 写口、内存回放器、落库器、测试替身。
//
// 契约（**sink 必须自己吞掉错误**：Emit 也会兜，但兜两次等于没有契约）：
//   - Write 失败要返回 error；
//   - **不要 panic**（Emitter 不 recover——一个会 panic 的 sink 是调用方的 bug，
//     藏起来只会在别处更难查）。
type Sink interface {
	Write(ev Event) error
}

// SinkFunc 让普通函数当 Sink 用。
type SinkFunc func(ev Event) error

func (f SinkFunc) Write(ev Event) error { return f(ev) }

// Health 是出口的健康报告（契约 3：降级必须可见）。
type Health struct {
	Delivered int            `json:"delivered"` // 成功送达
	Dropped   int            `json:"dropped"`   // 因 sink 报错而丢弃（**不阻断问答**）
	Rejected  int            `json:"rejected"`  // 事件自身不合法被拒（构造错/载荷缺）
	Reasons   map[string]int `json:"reasons,omitempty"`
}

// Emitter 拥有序号与时钟，并保证四条契约。它是**唯一**该被流程层持有的东西：
// 有了它，"要不要发事件"就是一行 `emit(ev)`，不必到处判断 sink 是否存在。
type Emitter struct {
	mu    sync.Mutex
	sink  Sink
	seq   int
	start time.Time
	he    Health
}

// NewEmitter 装一个出口。sink 为 nil 时是**合法的"没接"**（契约 1）。
func NewEmitter(sink Sink) *Emitter {
	return &Emitter{sink: sink, start: time.Now(), he: Health{Reasons: map[string]int{}}}
}

// Emit 发一个事件：补序号与时刻、校验、投递。
//
// 返回值语义要小心：
//   - nil   = 送达，**或** sink 缺席（没接不算错）；
//   - error = 事件本身不合法（调用方的 bug）；sink 报错**不算**（已计入
//     Health.Dropped，问答照常）。
func (e *Emitter) Emit(ev Event) error {
	if e == nil {
		return nil // 没装发射器 = 这一层没接上（缺席不改行为）
	}
	if err := ev.Validate(); err != nil {
		e.mu.Lock()
		e.he.Rejected++
		e.he.Reasons[err.Error()]++
		e.mu.Unlock()
		return err
	}
	e.mu.Lock()
	e.seq++
	ev.Seq = e.seq
	ev.AtMS = sinceMS(e.start)
	sink := e.sink
	e.mu.Unlock()
	if sink == nil {
		return nil // 契约 1：缺席不是错
	}
	if err := sink.Write(ev); err != nil {
		// 契约 3：降级不阻断，但计数。
		e.mu.Lock()
		e.he.Dropped++
		e.he.Reasons["sink: "+err.Error()]++
		e.mu.Unlock()
		return nil
	}
	e.mu.Lock()
	e.he.Delivered++
	e.mu.Unlock()
	return nil
}

// Health 返回出口健康（降级可见）。
func (e *Emitter) Health() Health {
	if e == nil {
		return Health{Delivered: 0, Dropped: 0, Rejected: 0, Reasons: map[string]int{"emitter": 0}}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.he
	out.Reasons = map[string]int{}
	for k, v := range e.he.Reasons {
		out.Reasons[k] = v
	}
	return out
}

// Events 返回已送达的事件数（进度读数用）。
func (e *Emitter) Events() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.he.Delivered
}

// --- 出口实现 ---

// Recorder 是内存出口：测试、离线回放、以及"先把事件收下来再决定给谁看"。
// 它的存在让"流程层发事件"这件事**不需要任何真实连接**（契约 1 的另一半）。
type Recorder struct {
	mu       sync.Mutex
	events   []Event
	failWith error // 非 nil 时 Write 一律失败（测降级用）
}

// NewRecorder 建一个内存出口。
func NewRecorder() *Recorder { return &Recorder{} }

// FailWith 让 Write 之后一律失败（降级路径的测试替身）。
func (r *Recorder) FailWith(err error) {
	r.mu.Lock()
	r.failWith = err
	r.mu.Unlock()
}

func (r *Recorder) Write(ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWith != nil {
		return r.failWith
	}
	r.events = append(r.events, ev)
	return nil
}

// All 返回全部事件（拷贝）。
func (r *Recorder) All() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Kinds 返回事件类型序列（顺序即真相的读数）。
func (r *Recorder) Kinds() []Kind {
	out := make([]Kind, 0, len(r.All()))
	for _, ev := range r.All() {
		out = append(out, ev.Kind)
	}
	return out
}

// AnswerText 拼出答案全文（content 事件按序拼接；replace=true 覆盖之前的内容）。
func (r *Recorder) AnswerText() string {
	var cur string
	for _, ev := range r.All() {
		if ev.Kind != KindContent || ev.Content == nil {
			continue
		}
		if ev.Content.Replace {
			cur = ev.Content.Text
			continue
		}
		cur += ev.Content.Text
	}
	return cur
}

// ReasoningText 拼出思考全文。
func (r *Recorder) ReasoningText() string {
	var b string
	for _, ev := range r.All() {
		if ev.Kind == KindReasoning && ev.Reasoning != nil {
			b += ev.Reasoning.Text
		}
	}
	return b
}

// SSEStream 是 **text/event-stream** 出口：沿用旧 cumulus 的帧形状
// （`event: <kind>` + `data: <json>`），所以老前端与通用 SSE 客户端都能直接吃。
//
// 不依赖 net/http：接受 io.Writer，因此可以在任何宿主里用（HTTP handler、
// 测试缓冲、CLI 管道）。
type SSEStream struct {
	mu      sync.Mutex
	w       io.Writer
	flusher func() // 可选：写完就冲（HTTP 用）
	failed  error
}

// NewSSE 建一个 SSE 出口。flusher 可为 nil（不冲）。
func NewSSE(w io.Writer, flusher func()) *SSEStream {
	return &SSEStream{w: w, flusher: flusher}
}

func (s *SSEStream) Write(ev Event) error {
	buf, err := Marshal(ev)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return s.failed // 写过一次失败之后不再尝试（半截的流比断流更难解释）
	}
	if _, err := s.w.Write(buf); err != nil {
		s.failed = err
		return err
	}
	if s.flusher != nil {
		s.flusher()
	}
	return nil
}

// KeepAlive 发一个 SSE 注释帧（心跳）。流式客户端靠它判活。
func (s *SSEStream) KeepAlive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.w.Write([]byte(": keep-alive\n\n"))
	if s.flusher != nil {
		s.flusher()
	}
	return err
}

// Marshal 把事件编码成一帧 SSE（契约 4：事件即数据，单帧自洽）。
func Marshal(ev Event) ([]byte, error) {
	buf, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	return append(append([]byte("event: "), ev.Kind...), append([]byte("\ndata: "), append(buf, '\n', '\n')...)...), nil
}

// ErrSinkClosed 给测试与实现方用的哨兵错误。
var ErrSinkClosed = errors.New("harness: sink closed")
