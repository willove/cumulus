package query

import (
	"strings"
	"sync"

	gocontext "context"

	"github.com/willove/cumulus/internal/llm"
)

// Cached 是词汇桥的缓存封装：同一个（归一化）问句的扩展结果只问模型
// 一次。
//
// 为什么必须：桥是整条链上最后一个非确定源。同一句"闯红灯怎么处罚"，
// 模型每次给的扩展词不同 → 检索不同 → 答案在"完整"与"未涉及"之间摆动
// （破局后验收时 12 跑里 f1 时对时不对，根因就在此）。缓存之后，同一
// 问题的第二次起行为完全一致——复用的是"翻译"不是"答案"，答案面该变
// 时（语料变了）缓存键不变但答案照常重新合成，语义上安全。
//
// 键的归一化：去首尾空白 + 小写。不改内部（中文无大小写，英文问句因
// 此受益）。
type Cached struct {
	Inner Expander

	mu    sync.Mutex
	items map[string][]string
	Hits  int // 缓存命中次数（可观测：第二次起同一问题不再烧扩展调用）
}

// Expand 实现 Expander：先查缓存，未命中才问内层并写入。
func (c *Cached) Expand(ctx gocontext.Context, question string) ([]string, error) {
	if c.Inner == nil {
		return nil, llm.ErrNotConfigured
	}
	key := normalizeQuestion(question)
	c.mu.Lock()
	if c.items == nil {
		c.items = map[string][]string{}
	}
	if got, ok := c.items[key]; ok {
		c.Hits++
		c.mu.Unlock()
		return got, nil
	}
	c.mu.Unlock()

	got, err := c.Inner.Expand(ctx, question)
	if err != nil {
		return nil, err // 失败不进缓存：下次还问（瞬态错误不该被钉住）
	}
	c.mu.Lock()
	c.items[key] = got
	c.mu.Unlock()
	return got, nil
}

// Hits 报告缓存命中次数。
func (c *Cached) HitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Hits
}

func normalizeQuestion(q string) string {
	return strings.ToLower(strings.TrimSpace(q))
}
