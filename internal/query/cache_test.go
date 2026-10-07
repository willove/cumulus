package query

import (
	"fmt"
	"sync/atomic"
	"testing"

	gocontext "context"
)

type countingExpander struct {
	calls int32
	out   []string
	err   error
}

func (c *countingExpander) Expand(gocontext.Context, string) ([]string, error) {
	atomic.AddInt32(&c.calls, 1)
	return c.out, c.err
}

// 同一问句只问模型一次：第二次走缓存（根因：桥是链上最后一个非确定
// 源，同一问题每次扩展词不同，检索与答案跟着摆）。
func TestCachedExpanderAsksOnce(t *testing.T) {
	inner := &countingExpander{out: []string{"交通违法", "记分"}}
	c := &Cached{Inner: inner}
	ctx := gocontext.Background()
	for i := 0; i < 3; i++ {
		got, err := c.Expand(ctx, "闯红灯怎么处罚")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "交通违法" {
			t.Fatalf("缓存必须返回同样的扩展：%v", got)
		}
	}
	if n := atomic.LoadInt32(&inner.calls); n != 1 {
		t.Fatalf("模型只该被问 1 次，实际 %d", n)
	}
	if c.HitCount() != 2 {
		t.Fatalf("命中该是 2，got %d", c.HitCount())
	}
}

// 失败不进缓存：瞬态错误下次还问（错误被钉住比多问一次贵）。
func TestCachedExpanderDoesNotCacheErrors(t *testing.T) {
	inner := &countingExpander{err: fmt.Errorf("llm down")}
	c := &Cached{Inner: inner}
	if _, err := c.Expand(gocontext.Background(), "q"); err == nil {
		t.Fatal("该报错")
	}
	inner.err = nil
	inner.out = []string{"x"}
	if _, err := c.Expand(gocontext.Background(), "q"); err != nil {
		t.Fatal("第二次该成功")
	}
	if n := atomic.LoadInt32(&inner.calls); n != 2 {
		t.Fatalf("失败后必须重新问，实际问了下层 %d 次", n)
	}
}

// 归一化：大小写/空白不同的同一问句共享缓存。
func TestCachedExpanderNormalizesKey(t *testing.T) {
	inner := &countingExpander{out: []string{"x"}}
	c := &Cached{Inner: inner}
	ctx := gocontext.Background()
	_, _ = c.Expand(ctx, "Patent Term")
	_, _ = c.Expand(ctx, "  patent term  ")
	if n := atomic.LoadInt32(&inner.calls); n != 1 {
		t.Fatalf("归一化后同一问句只问一次，实际 %d", n)
	}
}
