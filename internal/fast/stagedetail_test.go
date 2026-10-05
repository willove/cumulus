package fast

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// The stage events must carry WHAT each step did, not only that it ran:
// analyze's intent and terms, cascade's arm and top docs, sample's kept
// count and source. These payloads are what the UI timeline renders —
// pin their presence and shape (live case: 「闯红灯有什么处罚」 answered
// "where did the seconds go" but never "what was done").
func TestSearchStagesCarryDetail(t *testing.T) {
	body := "关键配置：连接池最大 128，超时 30 秒。\n" + strings.Repeat("填充 padding padding。\n", 40)
	src := source.New("配置手册", "md", "file://cfg", "cfg", "zh", body, nil)
	e := New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})

	var mu sync.Mutex
	details := map[string]any{}
	e.Stages = func(name string, _ time.Duration, detail any) {
		if detail == nil {
			return
		}
		mu.Lock()
		details[name] = detail
		mu.Unlock()
	}
	if _, err := e.Search(context.Background(), "连接池最大连接数", []source.Source{src}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	a, ok := details["analyze"].(map[string]any)
	if !ok {
		t.Fatalf("analyze detail missing or wrong shape: %#v", details["analyze"])
	}
	if a["intent"] == "" {
		t.Fatal("analyze detail must carry the intent")
	}
	c, ok := details["cascade"].(map[string]any)
	if !ok || c["arm"] == "" {
		t.Fatalf("cascade detail must carry the arm that hit: %#v", details["cascade"])
	}
	top, _ := c["top"].([]map[string]string)
	if len(top) == 0 || top[0]["t"] != "配置手册" || top[0]["id"] == "" {
		t.Fatalf("cascade detail's top docs must label the ranked source with a preview id: %#v", c["top"])
	}
	s, ok := details["sample"].(map[string]any)
	if !ok {
		t.Fatalf("sample detail missing: %#v", details["sample"])
	}
	if s["source"] != "配置手册" || s["kept"].(int) == 0 {
		t.Fatalf("sample detail must carry source and a nonzero kept count: %#v", s)
	}
	// synth has no payload — B's reasoning rides its own stream, not this one.
	if _, has := details["synth"]; has {
		t.Fatal("synth stage carries no detail by design")
	}
}
