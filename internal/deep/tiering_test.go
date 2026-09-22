package deep

import (
	"context"
	"testing"

	"github.com/cumubase/ask/internal/source"
)

// FILENAME_ONLY answers before retrieval and must never escalate.
func TestFilenameTierExits(t *testing.T) {
	e := newEngine()
	src := source.New("连接池专册", "md", "file://p", "pool", "zh", "连接池最大 128。", nil)
	res, err := e.Ask(context.Background(), "连接池专册", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated || res.Mode != "FILENAME_ONLY" {
		t.Fatalf("filename tier must exit: %+v", res)
	}
	if res.Answer.LLMCalls != 0 || res.Answer.SourceID != src.ID {
		t.Fatalf("filename answer wrong: %+v", res.Answer)
	}
}

// Chat intent is a tier exit — thin evidence must not drag it into DEEP.
func TestChatIntentDoesNotEscalate(t *testing.T) {
	e := newEngine()
	res, err := e.Ask(context.Background(), "你好", srcs())
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated || res.Mode != "CHAT" {
		t.Fatalf("chat must not escalate: %+v", res)
	}
}
