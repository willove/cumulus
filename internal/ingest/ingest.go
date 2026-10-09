// Package ingest 是摄入面：知识进系统的零摩擦通道。
//
// 研究笔记的结论（04）：个人库的竞争不在检索精度，在**摄入摩擦**——
// 主动维护型产品（分类树/标签/双链）大多没成，被动采集型是唯一活下来
// 的形态。所以这里的设计取向是：存进来只要一步，整理是系统的事。
//
// 三条通道：
//   - 粘贴：POST 一段文本；
//   - 链接：给一个 URL，取回正文（剥离标签，不做 readability 级提取——
//     够用且诚实，边界写在注释里）；
//   - 目录：文件落进看目录即入库（轮询，无外部依赖；.txt/.md 直接收，
//     .jsonl 走 corpus.ImportFile）。
//
// 去重是内容寻址的白拿：同样内容 → 同一个 id → upsert。存两遍不两份。
package ingest

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	gocontext "context"

	"github.com/willove/cumulus/internal/corpus"
)

// Text 存一段文本（粘贴/链接/看目录的单件入口）：过摄入管
// （corpus.Prepare：解码→规范化→内容寻址），血缘随文档落库。
// Text 摄入一段文本到 **realm 的集合**。realm 是显式参数而不是从 context 取：
// 漏传 realm 的后果是**写进别人的集合**（静默的跨租户污染），编译器能抓的
// 错误就不要留给人记。realm 为空 = 默认集合（单机老路径不变）。
func Text(ctx gocontext.Context, p corpus.Port, realm, body, source string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("ingest: empty body")
	}
	pre, err := corpus.Prepare([]byte(body))
	if err != nil {
		return "", fmt.Errorf("ingest: prepare: %w", err)
	}
	if err := p.EnsureCollection(ctx, corpus.CollectionFor(realm)); err != nil {
		return "", err
	}
	doc := corpus.Doc{
		ID:        pre.ID,
		Body:      pre.Body,
		Encoding:  pre.Encoding,
		SrcDigest: pre.SrcDigest,
		SrcBytes:  pre.SrcBytes,
	}
	_ = source // 来源留给调用方日志（id 是内容哈希，前缀不可行）
	if err := p.PutStruct(ctx, corpus.CollectionFor(realm), pre.ID, doc); err != nil {
		return "", fmt.Errorf("ingest: put %s: %w", pre.ID, err)
	}
	return pre.ID, nil
}

// FetchURL 取一个 URL 的正文。做法：GET，剥 script/style，去标签，
// 压空白。**不做**正文识别算法（readability 那种）——个人库以文档型
// 页面为主，简单剥离够用；遇到提取很差的内容，调用方自己贴文本更可靠。
func FetchURL(ctx gocontext.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("ingest: url %s: %w", url, err)
	}
	req.Header.Set("User-Agent", "cumulus-ingest/0.1")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ingest: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ingest: %s status %d", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("ingest: read %s: %w", url, err)
	}
	return StripHTML(string(raw)), nil
}

// StripHTML 做最朴素的正文提取：去 script/style 块，去标签，解三个
// 常见实体，压空白。段落边界保留成换行（列表与段落是个人知识的主体）。
func StripHTML(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if strings.HasPrefix(s[i:], "<!--") {
			if end := strings.Index(s[i:], "-->"); end >= 0 {
				i += end + 3
				continue
			}
		}
		if s[i] == '<' {
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				break
			}
			tag := strings.ToLower(s[i+1 : i+end])
			i += end + 1
			switch {
			case strings.HasPrefix(tag, "script"), strings.HasPrefix(tag, "style"):
				close := "</" + strings.Fields(tag)[0]
				if idx := strings.Index(strings.ToLower(s[i:]), close); idx >= 0 {
					i += idx + len(close)
				}
			case tag == "br" || tag == "li" || tag == "p" || strings.HasPrefix(tag, "h"):
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	text := b.String()
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&amp;", "&")
	// 压空白：行内多空格成一空格，行间多空行成一空行
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// WatchDir 轮询看目录：新文件（.txt/.md）或变化的文件入库，.jsonl 交给
// corpus.ImportFile。轮询无外部依赖（fsnotify 是一颗依赖，个人库的
// 摄入量级不需要事件精度）。每次回调 imported 增量。
func WatchDir(ctx gocontext.Context, p corpus.Port, realm, dir string, interval time.Duration, onChange func(imported int)) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	seen := map[string]string{} // path → content hash
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			imported := scanOnce(ctx, p, realm, dir, seen)
			if imported > 0 && onChange != nil {
				onChange(imported)
			}
		}
	}
}

func scanOnce(ctx gocontext.Context, p corpus.Port, realm, dir string, seen map[string]string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0 // 目录暂无/已删：等下一轮
	}
	imported := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		sum := corpus.DigestHex(raw) // 内容寻址同口径（规范化前，仅用于"变没变"判定）
		if seen[path] == sum {
			continue // 见过且没变
		}
		seen[path] = sum
		switch {
		case strings.HasSuffix(name, ".jsonl"):
			if n, err := corpus.ImportFileRealm(ctx, p, realm, path); err == nil {
				imported += n
			}
		case strings.HasSuffix(name, ".txt"), strings.HasSuffix(name, ".md"):
			body := string(raw)
			if strings.TrimSpace(body) == "" {
				continue
			}
			if _, err := Text(ctx, p, realm, body, name); err == nil {
				imported++
			}
		}
	}
	return imported
}
