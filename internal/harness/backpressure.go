package harness

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncSink 把事件交给**单独的写者协程**，于是"消费者慢"不再等于"流程卡住"。
//
// 为什么必须这样（真跑想到的漏洞）：`Emit` 里 `sink.Write(ev)` 是**同步内联**的，
// 而 io.Writer 的写**无法取消**——对端卡住时既不能超时返回，也不能放弃（放弃会
// 让下一次写与它交错，把流写坏）。所以正解不是"给写加超时"，而是**把写移出流程
// 协程**：流程只做入队（有界），写者独占 writer。
//
// 三条性质：
//
//  1. **顺序即真相**：单写者 + FIFO 队列 → 事件顺序与 Emit 顺序一致（harness 契约 2）。
//  2. **遥测帧可丢、交付帧必等**：进度类（stage/file/started/related）队列满即丢并
//     计数——丢一帧进度好过卡住整个问答；交付类（reasoning/content/citations/done/
//     error）等一会儿（默认 deliverWait），超时才丢。**答案可以等，不可以丢**。
//  3. **降级可见**：丢帧计入 Health（SlowDrops），收尾在 done.counts 里可查。
type AsyncSink struct {
	inner   Sink
	queue   chan Event
	deliver time.Duration

	closeOnce sync.Once
	closed    chan struct{}
	done      chan struct{}

	delivered atomic.Int64
	dropped   atomic.Int64
	enqueued  atomic.Int64
	writeErr  atomic.Value // error
}

// NewAsync 装一个异步写者。queue 是排队上限（0 = 默认 64）；
// deliverWait 是交付帧愿意等的时间（0 = 默认 5s）。
func NewAsync(inner Sink, queue int, deliverWait time.Duration) *AsyncSink {
	if queue <= 0 {
		queue = 64
	}
	if deliverWait <= 0 {
		deliverWait = 5 * time.Second
	}
	a := &AsyncSink{
		inner:   inner,
		queue:   make(chan Event, queue),
		deliver: deliverWait,
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go a.run()
	return a
}

// ErrAsyncClosed 队列已关（写者已收尾）。
var ErrAsyncClosed = errors.New("harness: async sink closed")

func (a *AsyncSink) run() {
	defer close(a.done)
	for ev := range a.queue {
		if err := a.inner.Write(ev); err != nil {
			a.writeErr.Store(err)
			a.dropped.Add(1)
		} else {
			a.delivered.Add(1)
		}
	}
}

// Write 入队。**它不会因为对端慢而阻塞流程**（遥测帧满即丢；交付帧最多等 deliver）。
func (a *AsyncSink) Write(ev Event) error {
	if isTelemetry(ev.Kind) {
		// 非阻塞投递：队列满 = 对端跟不上，丢一帧遥测并计数。
		select {
		case a.queue <- ev:
			a.enqueued.Add(1)
			return nil
		default:
			a.dropped.Add(1)
			return nil // 丢遥测不算错误（契约 3：降级不阻断）
		}
	}
	timer := time.NewTimer(a.deliver)
	defer timer.Stop()
	select {
	case a.queue <- ev:
		a.enqueued.Add(1)
		return nil
	case <-timer.C:
		a.dropped.Add(1)
		return nil // 交付帧等超时才丢，且同样不阻断
	case <-a.closed:
		return ErrAsyncClosed
	}
}

// Close 停写者（先排空队列，保证已入队的交付帧都写出去）。
func (a *AsyncSink) Close() error {
	a.closeOnce.Do(func() {
		close(a.closed)
		close(a.queue)
	})
	<-a.done
	if v := a.writeErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// Stats 是异步写的读数（丢帧必须看得见）。
type Stats struct {
	Enqueued  int64 `json:"enqueued"`
	Delivered int64 `json:"delivered"`
	Dropped   int64 `json:"dropped"` // 队列满/超时/写失败
}

// Stats 返回读数。
func (a *AsyncSink) Stats() Stats {
	return Stats{Enqueued: a.enqueued.Load(), Delivered: a.delivered.Load(), Dropped: a.dropped.Load()}
}

// Pending 是当前排队未写出的帧数（背压读数）。
func (a *AsyncSink) Pending() int { return len(a.queue) }

// isTelemetry 判"丢了也不影响交付"的帧。
//
// 判据是**丢了之后用户会不会少了要的东西**：进度/检索日志/关联文档是可再生的（下一
// 帧会覆盖上一帧的状态），思考/正文/引用/收尾是**唯一副本**，丢了就是丢内容。
func isTelemetry(k Kind) bool {
	switch k {
	case KindStage, KindFile, KindStarted, KindRelated:
		return true
	default:
		return false
	}
}
