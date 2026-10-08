package harness

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/store"
)

func memStore(t *testing.T) store.Port {
	t.Helper()
	st, err := store.Open("", true)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func ev(seq int, kind Kind) Event {
	e := Event{Seq: seq, Kind: kind, RunID: "s1"}
	switch kind {
	case KindStarted:
		e.Question = "问题"
	case KindContent:
		e.Content = &TextInfo{Text: "答案"}
	case KindStage:
		e.Stage = &StageInfo{Name: "evidence-supply", Phase: PhaseDone}
	case KindError:
		e.Error = "boom"
	}
	return e
}

func TestSessionLogRecordsAndReplays(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	log, err := OpenSession(ctx, st, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		if err := log.Record(ctx, ev(i, KindContent)); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Commit(ctx, false); err != nil {
		t.Fatal(err)
	}
	man, ok := SessionManifestOf(ctx, st, "s1")
	if !ok || man.Count != 4 || man.LastSeq != 4 || man.Complete {
		t.Fatalf("manifest wrong: %+v (ok=%v)", man, ok)
	}
	evs, man2, err := Replay(ctx, st, "s1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 || man2.Count != 4 {
		t.Fatalf("replay wrong: %d events, manifest %+v", len(evs), man2)
	}
	for i, e := range evs {
		if e.Seq != i+1 {
			t.Fatalf("replay must be ordered by seq: %d at %d", e.Seq, i)
		}
	}
	// 游标续传
	tail, _, err := Replay(ctx, st, "s1", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 || tail[0].Seq != 3 {
		t.Fatalf("cursor resume wrong: %d frames, first seq %d", len(tail), tail[0].Seq)
	}
	// limit
	page, _, err := Replay(ctx, st, "s1", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("limit must apply: %d", len(page))
	}
}

// 重复 Record 幂等：同 seq 覆盖，**清单数不虚增**（否则回放多出空位，客户端以为有缺口）。
func TestSessionLogRecordIsIdempotent(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	log, _ := OpenSession(ctx, st, "s2")
	for i := 1; i <= 3; i++ {
		if err := log.Record(ctx, ev(i, KindContent)); err != nil {
			t.Fatal(err)
		}
	}
	// 再录一遍 seq=3
	if err := log.Record(ctx, ev(3, KindContent)); err != nil {
		t.Fatal(err)
	}
	if err := log.Commit(ctx, false); err != nil {
		t.Fatal(err)
	}
	man, _ := SessionManifestOf(ctx, st, "s2")
	if man.Count != 3 || man.LastSeq != 3 {
		t.Fatalf("idempotent record must not inflate counts: %+v", man)
	}
	evs, _, _ := Replay(ctx, st, "s2", 0, 0)
	if len(evs) != 3 {
		t.Fatalf("replay after duplicate: %d frames", len(evs))
	}
}

// 同一 session 二次打开 = **续写**（不是重开）：游标从清单恢复。
func TestSessionLogResumesAcrossOpens(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	first, _ := OpenSession(ctx, st, "s3")
	for i := 1; i <= 2; i++ {
		_ = first.Record(ctx, ev(i, KindContent))
	}
	_ = first.Commit(ctx, false)

	second, _ := OpenSession(ctx, st, "s3")
	for i := 3; i <= 5; i++ {
		_ = second.Record(ctx, ev(i, KindContent))
	}
	_ = second.Commit(ctx, true)

	man, _ := SessionManifestOf(ctx, st, "s3")
	if man.Count != 5 || !man.Complete {
		t.Fatalf("resume must continue, not reset: %+v", man)
	}
	evs, _, _ := Replay(ctx, st, "s3", 0, 0)
	if len(evs) != 5 || evs[4].Seq != 5 {
		t.Fatalf("resumed session replay wrong: %d frames", len(evs))
	}
}

// done/error 帧自动提交清单（complete=true）：会话"完了"这件事不靠调用方记得调。
func TestSessionLogAutoCommitsOnTerminalFrame(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	log, _ := OpenSession(ctx, st, "s4")
	_ = log.Record(ctx, ev(1, KindStarted))
	_ = log.Record(ctx, ev(2, KindDone))
	man, ok := SessionManifestOf(ctx, st, "s4")
	if !ok || !man.Complete {
		t.Fatalf("terminal frame must commit the manifest: %+v (ok=%v)", man, ok)
	}
	// 没提交的会话读不到清单（半程被杀不冒充完整）
	if _, ok := SessionManifestOf(ctx, memStore(t), "s4"); ok {
		t.Fatal("a session without manifest must not be readable")
	}
}

// 查不到的会话要报错（不是空列表）——空列表会让客户端以为"没事件"。
func TestReplayUnknownSessionErrors(t *testing.T) {
	if _, _, err := Replay(context.Background(), memStore(t), "nope", 0, 0); err == nil {
		t.Fatal("unknown session must error")
	}
}

// nil store = 没挂持久化：缺席是合法状态，不崩。
func TestSessionLogNilStoreIsSafe(t *testing.T) {
	ctx := context.Background()
	log, err := OpenSession(ctx, nil, "s5")
	if err == nil {
		t.Fatal("opening without a store must say so")
	}
	if log != nil {
		t.Fatal("no store → nil log")
	}
	if _, ok := SessionManifestOf(ctx, nil, "s5"); ok {
		t.Fatal("nil store must report no manifest")
	}
	var nilLog *SessionLog
	if err := nilLog.Record(ctx, ev(1, KindContent)); err != nil {
		t.Fatalf("nil log must be a no-op: %v", err)
	}
}

// 边发边录：live 与回放是**同一批事件**（两处消费不可能不一致）。
func TestRecordingSinkSendsAndRecords(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	log, _ := OpenSession(ctx, st, "s6")
	rec := NewRecorder()
	sink := NewRecording(rec, log)
	em := NewEmitter(sink)
	for i := 1; i <= 3; i++ {
		if err := em.Emit(ev(i, KindContent)); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.All()) != 3 {
		t.Fatalf("live must still receive: %d", len(rec.All()))
	}
	if err := log.Commit(ctx, false); err != nil {
		t.Fatal(err)
	}
	evs, _, _ := Replay(ctx, st, "s6", 0, 0)
	if len(evs) != 3 {
		t.Fatalf("recorded copy must be replayable: %d", len(evs))
	}
	for i := range evs {
		if evs[i].Seq != rec.All()[i].Seq || evs[i].Kind != rec.All()[i].Kind {
			t.Fatalf("recorded and live copies diverged at %d", i)
		}
	}
	// 只录不发（离线评测只要历史）
	logOnly, _ := OpenSession(context.Background(), st, "s7")
	silent := NewRecording(nil, logOnly)
	if err := silent.Write(ev(1, KindContent)); err != nil {
		t.Fatal(err)
	}
}
