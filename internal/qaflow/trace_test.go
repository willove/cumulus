package qaflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/harness"
	"github.com/willove/cumulus/internal/retrieval"
)

var errFail = errors.New("取数失败")

func traceCorpus() *retrieval.Index {
	return retrieval.Build([]retrieval.Document{
		{ID: "d1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "d2", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。变更窗口在周二凌晨。"},
	})
}

func traceSynth(_ string, ws []EvidenceWindow, _ facts.Report) (Answer, Usage, error) {
	a := Answer{Text: "最大连接数是 100"}
	if len(ws) > 0 {
		a.Citations = []string{ws[0].SourceID + "#" + ws[0].Span}
	}
	return a, Usage{}, nil
}

// 挂上发射器 → 阶段与窗口都进事件流，且顺序是流程顺序（证据先于合成）。
func TestTraceEmitsStagesAndWindows(t *testing.T) {
	rec := harness.NewRecorder()
	r := Runner("连接池最大连接数是多少", BM25Evidence(traceCorpus(), 3, 60), traceSynth, Options{
		Emitter: harness.NewEmitter(rec), RunID: "run-t1",
	})
	c := context.New("trace-test")
	if err := r.Run(c); err != nil {
		t.Fatal(err)
	}
	evs := rec.All()
	if len(evs) == 0 {
		t.Fatal("emitter attached but no events")
	}
	var sawStageStart, sawFile, sawSynth bool
	for _, ev := range evs {
		if ev.RunID != "run-t1" {
			t.Fatalf("run id must ride on every frame: %+v", ev)
		}
		switch {
		case ev.Kind == harness.KindStage && ev.Stage.Name == (EvidenceStage{}).Name() && ev.Stage.Phase == harness.PhaseStart:
			sawStageStart = true
		case ev.Kind == harness.KindStage && ev.Stage.Name == (SynthesizeStage{}).Name():
			if sawFile {
				sawSynth = true
			}
		case ev.Kind == harness.KindFile:
			sawFile = true
			if ev.File.DocID == "" || ev.File.Rank <= 0 || ev.File.Span == "" {
				t.Fatalf("file frame must be self-sufficient: %+v", ev.File)
			}
		}
	}
	if !sawStageStart || !sawFile || !sawSynth {
		t.Fatalf("expected stage/file/synthesize frames; got %d frames: stage=%v file=%v synth=%v",
			len(evs), sawStageStart, sawFile, sawSynth)
	}
}

// **契约 1 的集成版**：没挂发射器时，答案与提交视图逐字段不变。
func TestTraceAbsentKeepsBehaviourIdentical(t *testing.T) {
	off := Runner("连接池最大连接数是多少", BM25Evidence(traceCorpus(), 3, 60), traceSynth, Options{
		CorpusVersion: "cv", ConfigVersion: "cfg", StrategyVersion: "sv", BeliefVersion: "none",
	})
	cOff := context.New("no-emitter")
	if err := off.Run(cOff); err != nil {
		t.Fatal(err)
	}
	aOff, _ := context.Get(cOff, KeyAnswer)
	viewOff := off.View

	rec := harness.NewRecorder()
	on := Runner("连接池最大连接数是多少", BM25Evidence(traceCorpus(), 3, 60), traceSynth, Options{
		CorpusVersion: "cv", ConfigVersion: "cfg", StrategyVersion: "sv", BeliefVersion: "none",
		Emitter: harness.NewEmitter(rec), RunID: "run-t2",
	})
	cOn := context.New("with-emitter")
	if err := on.Run(cOn); err != nil {
		t.Fatal(err)
	}
	aOn, _ := context.Get(cOn, KeyAnswer)
	viewOn := on.View

	if aOff.Text != aOn.Text || aOff.Refused != aOn.Refused {
		t.Fatalf("emitter must not change the answer: %+v vs %+v", aOff, aOn)
	}
	if viewOff.CorpusVersion != viewOn.CorpusVersion || viewOff.Flow != viewOn.Flow ||
		viewOff.StrategyVersion != viewOn.StrategyVersion {
		t.Fatalf("emitter must not change the committed view: %+v vs %+v", viewOff, viewOn)
	}
	if len(rec.All()) == 0 {
		t.Fatal("attached emitter produced nothing")
	}
}

// 失败也要留痕：阶段以 fail 相位收尾（耗时可见），错误由上层发 error 事件。
func TestTraceReportsStageFailure(t *testing.T) {
	rec := harness.NewRecorder()
	bad := func(*context.Context, Rewrite) ([]EvidenceWindow, error) {
		return nil, errFail
	}
	r := Runner("问题", bad, traceSynth, Options{Emitter: harness.NewEmitter(rec), RunID: "run-t3"})
	c := context.New("trace-fail")
	if err := r.Run(c); err == nil {
		t.Fatal("expected the flow to fail")
	}
	var sawFail bool
	for _, ev := range rec.All() {
		if ev.Kind == harness.KindStage && ev.Stage.Name == (EvidenceStage{}).Name() && ev.Stage.Phase == harness.PhaseDone {
			sawFail = true // 失败也发 done（只有 start 的阶段会让 UI 永远转圈）
		}
	}
	if !sawFail {
		t.Fatalf("failed stage must close its timing frame: %+v", rec.Kinds())
	}
}

// 关联文档：只列**没被引用**的窗口，且 why 说清理由。
func TestRelatedFromOnlyUncited(t *testing.T) {
	ws := []EvidenceWindow{
		{SourceID: "d1", Title: "一", Span: "rune[0:1]", Score: 3, Text: "命中"},
		{SourceID: "d2", Title: "二", Span: "rune[0:1]", Score: 2, Text: "没命中"},
		{SourceID: "d3", Title: "三", Span: "rune[0:1]", Score: 1, Text: "没命中"},
	}
	got := RelatedFrom(ws, []string{"d1#rune[0:1]"}, 5)
	if len(got) != 2 {
		t.Fatalf("related must skip cited docs: %+v", got)
	}
	for _, r := range got {
		if r.DocID == "d1" {
			t.Fatal("cited doc must not appear as related")
		}
		if !strings.Contains(r.Why, "uncited") {
			t.Fatalf("related must say why: %+v", r)
		}
	}
	if len(RelatedFrom(ws, []string{"d1#rune[0:1]", "d2#rune[0:1]", "d3#rune[0:1]"}, 5)) != 0 {
		t.Fatal("everything cited → no related")
	}
	if n := len(RelatedFrom(ws, nil, 1)); n != 1 {
		t.Fatalf("limit must apply: %d", n)
	}
}

// 提交视图压串：四版本 + 档位/阈值要能拼出来给外部对账。
func TestCommittedStringCarriesVersions(t *testing.T) {
	got := CommittedString(context.CommittedView{
		CorpusVersion: "a1", ConfigVersion: "c2", StrategyVersion: "s3", BeliefVersion: "none",
		Calibration: context.Calibration{Tier: "retrieval", Program: "hand-set", Threshold: 0.5, ThresholdVersion: "t1"},
	})
	for _, want := range []string{"corpus=a1", "config=c2", "strategy=s3", "belief=none", "tier=retrieval", "calibration=hand-set", "threshold=0.500", "threshold_version=t1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("committed string missing %q: %s", want, got)
		}
	}
}
