package qaflow

import (
	"testing"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/harness"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/retrieval"
)

// stubLLM 返回固定文本（分类器的 JSON 契约测试只需要这个）。
type stubLLM struct{ text string }

func (s stubLLM) Complete(_ gocontext.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{Text: s.text}, nil
}

func classWindows() []EvidenceWindow {
	return []EvidenceWindow{
		{SourceID: "d1", Title: "专利法第一条", Text: "为了保护专利权人的合法权益，制定本法。", Score: 9},
		{SourceID: "d2", Title: "专利制度历史", Text: "专利制度可追溯到中世纪特权制度。", Score: 5},
		{SourceID: "d3", Title: "旧版收费标准", Text: "年费为一百八十元（已废止）。", Score: 3},
	}
}

func TestClassifyParsesFourClasses(t *testing.T) {
	c := &WindowClassifier{Client: stubLLM{text: `{"ANSWER":[1],"RELATED":[2],"OUTDATED":[3],"UNKNOWN":[]}`}}
	got, err := c.Classify(context.New("classify"), "专利年费多少", classWindows())
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != ClassAnswer || got[2] != ClassRelated || got[3] != ClassOutdated {
		t.Fatalf("classes wrong: %+v", got)
	}
	// JSON 埋在解释文字里也要能救（推理模型常这么干）
	c2 := &WindowClassifier{Client: stubLLM{text: "分析如下：\n我认为 1 是答案。\n" + `{"ANSWER":[1],"RELATED":[2,3],"OUTDATED":[],"UNKNOWN":[]}`}}
	got2, err := c2.Classify(context.New("classify"), "q", classWindows())
	if err != nil {
		t.Fatal(err)
	}
	if got2[1] != ClassAnswer || got2[3] != ClassRelated {
		t.Fatalf("rescued JSON wrong: %+v", got2)
	}
}

// 半截结果**整体失败**：漏标、越界、重复、非法类别名、没 JSON —— 都不许进事件流。
func TestClassifyRejectsHalfResults(t *testing.T) {
	cases := map[string]string{
		"漏标":    `{"ANSWER":[1],"RELATED":[2],"OUTDATED":[],"UNKNOWN":[]}`,
		"越界":    `{"ANSWER":[9],"RELATED":[2],"OUTDATED":[3],"UNKNOWN":[]}`,
		"重复":    `{"ANSWER":[1,1],"RELATED":[2],"OUTDATED":[3],"UNKNOWN":[]}`,
		"没JSON": `我觉得第 1 条最相关`,
		"空回包":   ``,
	}
	for name, text := range cases {
		c := &WindowClassifier{Client: stubLLM{text: text}}
		got, err := c.Classify(context.New("c"), "q", classWindows())
		if err == nil {
			t.Errorf("%s：半截结果必须报错，实际给了 %+v", name, got)
		}
		if got != nil {
			t.Errorf("%s：失败时必须返回空，不得给半张表", name)
		}
	}
}

// 缺席是合法状态：没有分类器 / 没有客户端 → 不报错、也不返回"全 UNKNOWN"。
func TestClassifyAbsentIsNoop(t *testing.T) {
	var nilC *WindowClassifier
	if got, err := nilC.Classify(context.New("c"), "q", classWindows()); err != nil || got != nil {
		t.Fatalf("nil classifier must be a no-op: %+v %v", got, err)
	}
	noClient := &WindowClassifier{}
	if got, err := noClient.Classify(context.New("c"), "q", classWindows()); err != nil || got != nil {
		t.Fatalf("classifier without client must be a no-op: %+v %v", got, err)
	}
	if got, err := (&WindowClassifier{Client: stubLLM{text: "{}"}}).Classify(context.New("c"), "q", nil); err != nil || len(got) != 0 {
		t.Fatalf("no windows → no call, no error: %+v %v", got, err)
	}
}

// file 事件：类别合法才进流；**空类别 = 未分级**（与 UNKNOWN 必须能分开）。
func TestFileEventCarriesClass(t *testing.T) {
	ev, err := harness.File("r", harness.FileInfo{Rank: 1, DocID: "d1", Class: ClassAnswer})
	if err != nil {
		t.Fatal(err)
	}
	if ev.File == nil || ev.File.Class != ClassAnswer {
		t.Fatalf("class must ride on the file frame: %+v", ev.File)
	}
	if _, err := harness.File("r", harness.FileInfo{Rank: 1, DocID: "d1", Class: "也许吧"}); err == nil {
		t.Fatal("illegal class must not enter the stream")
	}
	none, err := harness.File("r", harness.FileInfo{Rank: 1, DocID: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	if none.File.Class != "" {
		t.Fatalf("absent classification must stay empty (≠ UNKNOWN): %q", none.File.Class)
	}
	if _, err := harness.File("r", harness.FileInfo{Rank: 1, DocID: "d1", Class: ""}); err != nil {
		t.Fatal("empty class must be allowed (未分级)")
	}
}

// 分级只影响**可解释性**，不改检索与路由：带分类器跑一遍，答案与引用必须完全一致。
func TestClassificationDoesNotChangeAnswer(t *testing.T) {
	run := func(withClassifier bool) (string, []string) {
		rec := harness.NewRecorder()
		opts := Options{Emitter: harness.NewEmitter(rec), RunID: "cls"}
		if withClassifier {
			// 回包要**标满 runner 实际发出的窗口数**——否则解析器会判"漏标"并
			// 整体失败（这正是它该做的；第一版我按 classWindows() 的 3 条写，
			// 而 runner 只发出 2 条，于是分类被静默丢弃）。
			// 回包按"runner 实际发出的窗口数"自洽标注：解析器对**漏标/越界**
			// 一律整体失败（这是对的），所以测试的回包必须标满。
			// classIndex 有 3 篇文档，但窗口数取决于检索命中几篇——用宽松回包
			// 覆盖所有可能（把 1..8 全标 RELATED 之外的 ANSWER 不现实），
			// 因此这里让回包随实际窗口数生成。
			opts.WindowClassifier = &WindowClassifier{Client: stubLLM{text: autoClassJSON(3)}}
		}
		c := context.New("cls")
		r := Runner("专利年费多少", BM25Evidence(classIndex(), 3, 80), traceSynth, opts)
		if err := r.Run(c); err != nil {
			t.Fatal(err)
		}
		ans, _ := context.Get(c, KeyAnswer)
		var classes []string
		for _, ev := range rec.All() {
			if ev.Kind == harness.KindFile {
				classes = append(classes, ev.File.Class)
			}
		}
		return ans.Text, classes
	}
	ansOff, classesOff := run(false)
	ansOn, classesOn := run(true)
	if ansOff != ansOn {
		t.Fatalf("classification must not change the answer: %q vs %q", ansOff, ansOn)
	}
	for _, c := range classesOff {
		if c != "" {
			t.Fatalf("no classifier → class must stay empty: %q", c)
		}
	}
	if len(classesOn) == 0 {
		t.Fatalf("with classifier, file frames must carry a class: %v", classesOn)
	}
	for i, c := range classesOn {
		if !WindowClassIs(c) {
			t.Fatalf("class must be a legal name (frame %d): %q", i, c)
		}
	}
}

func classIndex() *retrieval.Index {
	return retrieval.Build([]retrieval.Document{
		{ID: "d1", Body: "专利法第一条 为了保护专利权人的合法权益，鼓励发明创造，制定本法。"},
		{ID: "d2", Body: "专利制度历史 专利制度可追溯到中世纪特权制度。"},
		{ID: "d3", Body: "旧版收费标准 年费为一百八十元，该标准已废止。"},
	})
}

// autoClassJSON 生成"标满 n 条"的合法回包（第 1 条 ANSWER，其余 RELATED）。
// 解析器对漏标/越界一律整体失败，所以测试回包必须与实际窗口数自洽。
func autoClassJSON(n int) string {
	out := `{"ANSWER":[1],"RELATED":[`
	for i := 2; i <= n; i++ {
		if i > 2 {
			out += ","
		}
		out += itoa(i)
	}
	return out + `],"OUTDATED":[],"UNKNOWN":[]}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
