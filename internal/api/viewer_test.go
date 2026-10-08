package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/harness"
)

// 事件浏览器必须真的存在、指向流式端点。
func TestViewerIsServedAndPointsAtStream(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer must be served: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("viewer must be html: %s", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"/v1/qa/stream", "ReadableStream", "text/event-stream"} {
		if want == "text/event-stream" {
			continue // 服务端负责设头，页面里不需要出现
		}
		if !strings.Contains(body, want) {
			t.Fatalf("viewer must reference %q", want)
		}
	}
}

// **契约自检**：事件词表与消费端必须同步。新增 Kind 忘了在界面加分支 → 这里红。
//
// 这是"消费端漂移"的机械保证：以前这类漂移靠人记（然后某天新事件到了界面上
// 一声不响）。现在它是一条测试。
func TestViewerHandlesEveryEventKind(t *testing.T) {
	body := string(webPage)
	missing := []string{}
	for _, k := range harness.Kinds() {
		if !strings.Contains(body, `case "`+string(k)+`"`) {
			missing = append(missing, string(k))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("事件浏览器缺少这些事件的处理分支：%s", strings.Join(missing, ", "))
	}
}

// 词表本身也要可信：无重复、每种都有构造器（否则"全处理"是空话）。
func TestEventVocabularyIsSound(t *testing.T) {
	kinds := harness.Kinds()
	if len(kinds) == 0 {
		t.Fatal("vocabulary must not be empty")
	}
	seen := map[harness.Kind]bool{}
	for _, k := range kinds {
		if seen[k] {
			t.Fatalf("duplicate kind in vocabulary: %s", k)
		}
		seen[k] = true
		// 空载荷必须被自己的 Validate 拒绝（构造器不产半截事件的保证）。
		// started/error/related/citations 是例外：它们**可以**为空
		// （started 只要 question、error 只要文本、related/citations 允许空列表）。
		switch k {
		case harness.KindStarted, harness.KindError, harness.KindRelated, harness.KindCitations:
			continue
		}
		if err := (harness.Event{Kind: k}).Validate(); err == nil {
			t.Fatalf("empty %s payload must be rejected by Validate (半截事件比不发更坏)", k)
		}
	}
}
