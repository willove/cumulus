// Package corpus 是语料的装卸面：从 store 读、往 store 写、从目录导入。
//
// 检索侧的语料一直是内联或从 benchmark 文件来的——HTTP 面要求语料活在
// store 里（端口先行的另一面：collection 是语料的家，索引只是它的投影）。
// 索引可弃、语料不可弃：进程重启后从 store 重建索引，不需要重新导入。
package corpus

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gocontext "context"

	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/store"
)

// Collection 是语料在 store 里的集合名（realm 维度由 store 自己管）。
const Collection = "documents"

// Doc 是一篇语料文档（store 里的结构体形状）。
type Doc struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// Port 是语料面依赖的 store 能力（窄接口，测试可替）。它就是
// store.Port 的子集——不重新声明，直接用 store.Port 的类型别名语义，
// 测试给个假件即可。
type Port = store.Port

// Load 从 store 读全量语料。空集合返回空切片（不报错——空库是合法状态）。
func Load(ctx gocontext.Context, p Port) ([]retrieval.Document, error) {
	ids, err := p.ListIDs(ctx, Collection, 0)
	if err != nil {
		return nil, fmt.Errorf("corpus: list: %w", err)
	}
	docs := make([]retrieval.Document, 0, len(ids))
	for _, id := range ids {
		var d Doc
		if err := p.GetStruct(ctx, Collection, id, &d); err != nil {
			return nil, fmt.Errorf("corpus: get %s: %w", id, err)
		}
		if d.ID == "" {
			d.ID = id
		}
		docs = append(docs, retrieval.Document{ID: d.ID, Body: d.Body})
	}
	return docs, nil
}

// Save 把语料写进 store（upsert）。
func Save(ctx gocontext.Context, p Port, docs []retrieval.Document) error {
	if err := p.EnsureCollection(ctx, Collection); err != nil {
		return fmt.Errorf("corpus: ensure: %w", err)
	}
	for _, d := range docs {
		if err := p.PutStruct(ctx, Collection, d.ID, Doc{ID: d.ID, Body: d.Body}); err != nil {
			return fmt.Errorf("corpus: put %s: %w", d.ID, err)
		}
	}
	return nil
}

// ImportFile 从 JSONL 导入（每行 {"id": "...", "body": "..."}；也认
// cn-law-rag 的 anchor/positive 形状——positive 当 body，标题进 id 前缀）。
// 返回导入的文档数。已存在的 id 覆盖（upsert）。
func ImportFile(ctx gocontext.Context, p Port, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("corpus: open %s: %w", path, err)
	}
	defer f.Close()
	if err := p.EnsureCollection(ctx, Collection); err != nil {
		return 0, fmt.Errorf("corpus: ensure: %w", err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec struct {
			ID       string `json:"id"`
			Body     string `json:"body"`
			Positive string `json:"positive"` // cn-law-rag 形状
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return n, fmt.Errorf("corpus: %s: %w", path, err)
		}
		body := rec.Body
		if body == "" {
			body = rec.Positive
		}
		if body == "" {
			continue
		}
		id := rec.ID
		if id == "" {
			id = hashID(body)
		}
		if err := p.PutStruct(ctx, Collection, id, Doc{ID: id, Body: body}); err != nil {
			return n, fmt.Errorf("corpus: put %s: %w", id, err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return n, fmt.Errorf("corpus: scan %s: %w", path, err)
	}
	return n, nil
}

// ImportDir 导入目录下全部 .jsonl（按文件名排序，确定性）。
func ImportDir(ctx gocontext.Context, p Port, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("corpus: read dir %s: %w", dir, err)
	}
	total := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		n, err := ImportFile(ctx, p, filepath.Join(dir, e.Name()))
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func hashID(s string) string {
	const hex = "0123456789abcdef"
	var b [8]byte
	for i := 0; i < len(s); i++ {
		b[i%8] ^= s[i]
	}
	out := make([]byte, 16)
	for i, x := range b {
		out[i*2] = hex[x>>4]
		out[i*2+1] = hex[x&0xf]
	}
	return string(out)
}
