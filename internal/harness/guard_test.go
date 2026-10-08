package harness

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowWriter 模拟"对端卡住"：每次写都睡一会儿。
type slowWriter struct {
	mu    sync.Mutex
	got   []Event
	delay time.Duration
}

func (s *slowWriter) Write(ev Event) error {
	time.Sleep(s.delay) // 真 io.Writer 的写**不可取消**：这正是不能靠超时解决的问题
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, ev)
	return nil
}

func (s *slowWriter) events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.got...)
}

// 慢消费者**不得**拖住流程：200 帧遥测 + 一个 30ms 的慢写者，
// Emit 全程必须在毫秒级返回（不能按帧数 × delay 线性增长）。
func TestAsyncSinkDoesNotBlockTheFlow(t *testing.T) {
	sw := &slowWriter{delay: 30 * time.Millisecond}
	async := NewAsync(sw, 8, time.Second)
	em := NewEmitter(async)

	start := time.Now()
	for i := 0; i < 200; i++ {
		ev, _ := StageDetailed("r", "evidence-supply", PhaseDone, 1, "命中", float64(i))
		if err := em.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	emitCost := time.Since(start)

	if emitCost > 400*time.Millisecond {
		t.Fatalf("flow must not follow the slow consumer: 200 frames cost %v", emitCost)
	}
	if st := async.Stats(); st.Enqueued == 0 {
		t.Fatal("frames should have been enqueued")
	}
	_ = async.Close()
	// 丢帧是允许的，但**必须计数**（降级可见）
	if async.Stats().Dropped == 0 {
		t.Fatal("a slow consumer must produce counted drops, not silent loss")
	}
}

// 交付帧（content/citations/done）**可以等，不可以丢**。
func TestAsyncSinkDeliversPayloadFrames(t *testing.T) {
	sw := &slowWriter{delay: 2 * time.Millisecond}
	async := NewAsync(sw, 64, 2*time.Second)
	em := NewEmitter(async)
	for i := 0; i < 20; i++ {
		ev, _ := Content("r", "答案片段", false)
		if err := em.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := async.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(sw.events()); n != 20 {
		t.Fatalf("payload frames must not be dropped: %d/20", n)
	}
}

// 顺序即真相：异步写**仍然**保持 Emit 顺序（单写者 + FIFO）。
func TestAsyncSinkPreservesOrder(t *testing.T) {
	sw := &slowWriter{}
	async := NewAsync(sw, 128, time.Second)
	em := NewEmitter(async)
	for i := 0; i < 50; i++ {
		ev, _ := Content("r", string(rune('a'+i%26)), false)
		if err := em.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := async.Close(); err != nil {
		t.Fatal(err)
	}
	got := sw.events()
	if len(got) != 50 {
		t.Fatalf("want 50 frames, got %d", len(got))
	}
	for i, ev := range got {
		if ev.Seq != i+1 {
			t.Fatalf("async writer must preserve order: frame %d has seq %d", i, ev.Seq)
		}
	}
}

// Close 排空队列：已入队的交付帧在 Close 之后一定写出去（否则 done 帧会丢）。
func TestAsyncSinkCloseDrainsQueue(t *testing.T) {
	sw := &slowWriter{}
	async := NewAsync(sw, 256, time.Second)
	em := NewEmitter(async)
	for i := 0; i < 100; i++ {
		ev, _ := Done("r", DoneInfo{Answer: "a"})
		if err := em.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := async.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(sw.events()); n != 100 {
		t.Fatalf("Close must drain: %d/100", n)
	}
}

// schema 版本：消费方遇未知主版本要**显式失败**，不许静默解成零值。
func TestDecodeFrameRejectsUnknownVersion(t *testing.T) {
	ev, _ := Content("r", "答案", true)
	ev.Seq, ev.V = 1, SchemaVersion
	data, err := marshalForTest(ev)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeFrame(data)
	if err != nil || got.Content == nil || got.Content.Text != "答案" {
		t.Fatalf("current version must decode: %+v %v", got, err)
	}
	// 未来版本：显式报错
	bad := strings.Replace(string(data), `"v":1`, `"v":99`, 1)
	if _, err := DecodeFrame([]byte(bad)); err == nil {
		t.Fatal("unknown schema version must fail loudly, not decode to zero values")
	} else if !strings.Contains(err.Error(), "版本") {
		t.Fatalf("error should name the version problem: %v", err)
	}
}

// 会话保留期：到期能被清掉，没到期不动；续写不重置到期时刻。
func TestSessionRetention(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()

	// 到期
	log1, _ := OpenSessionTTL(ctx, st, "old", time.Millisecond)
	for i := 1; i <= 3; i++ {
		_ = log1.Record(ctx, ev(i, KindContent))
	}
	_ = log1.Commit(ctx, true)
	time.Sleep(5 * time.Millisecond)

	// 未到期
	log2, _ := OpenSessionTTL(ctx, st, "fresh", time.Hour)
	for i := 1; i <= 2; i++ {
		_ = log2.Record(ctx, ev(i, KindContent))
	}
	_ = log2.Commit(ctx, true)

	sessions, events, err := PruneExpired(ctx, st, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || events != 3 {
		t.Fatalf("prune should remove exactly the expired session: %d sessions, %d events", sessions, events)
	}
	if _, ok := SessionManifestOf(ctx, st, "old"); ok {
		t.Fatal("expired session must be gone")
	}
	if _, ok := SessionManifestOf(ctx, st, "fresh"); !ok {
		t.Fatal("fresh session must survive")
	}
	// 续写不重置到期时刻
	man1, _ := SessionManifestOf(ctx, st, "fresh")
	log3, _ := OpenSessionTTL(ctx, st, "fresh", time.Hour)
	_ = log3.Record(ctx, ev(3, KindContent))
	_ = log3.Commit(ctx, true)
	man2, _ := SessionManifestOf(ctx, st, "fresh")
	if man1.ExpiresAt != man2.ExpiresAt {
		t.Fatalf("resuming must not extend lifetime: %q → %q", man1.ExpiresAt, man2.ExpiresAt)
	}
}

// 显式删除（用户点了"清除记录"）走同一条路径。
func TestPruneSessionDeletes(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	log, _ := OpenSessionTTL(ctx, st, "del", time.Hour)
	for i := 1; i <= 5; i++ {
		_ = log.Record(ctx, ev(i, KindContent))
	}
	_ = log.Commit(ctx, true)
	n, err := PruneSession(ctx, st, "del")
	if err != nil || n != 5 {
		t.Fatalf("prune: %d events, %v", n, err)
	}
	if _, ok := SessionManifestOf(ctx, st, "del"); ok {
		t.Fatal("session must be gone after prune")
	}
	if n2, _ := PruneSession(ctx, st, "del"); n2 != 0 {
		t.Fatal("pruning an absent session must be a no-op (idempotent)")
	}
}

func marshalForTest(ev Event) ([]byte, error) { return json.Marshal(ev) }
