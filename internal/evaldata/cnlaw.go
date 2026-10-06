// Package evaldata 装真实评测语料。第一个来源是 cn-law-rag 的微调
// 数据集（65,783 条“口语问句 → 法条”），它同时提供两样东西：
//   - 语料：全部唯一法条（positive）——检索宇宙；
//     -题集：anchor（口语问句）+ 金标（该问句对应的法条 id）。
//
// 这个数据集是 cumulus 当年实测 BM25 块级 R@4 = 99.0% / MiniLM 1.1%
// 的同一个场景——向量模型在法律领域的老战场。
package evaldata

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/retrieval"
)

// CNLawRecord 是 finetune_dataset.jsonl 的一行。
type CNLawRecord struct {
	Anchor   string `json:"anchor"`
	Positive string `json:"positive"`
	Negative string `json:"negative"`
}

// CNLawSet 是装好的语料 + 题集 + 内容指纹。
type CNLawSet struct {
	Docs      []retrieval.Document
	Items     []evalfcore.Item
	CorpusSHA string
	ItemsSHA  string
}

// LoadCNLaw 流式读取数据集：
//   - 语料 = 全部唯一 positive（按全文去重），id = 内容哈希前 12 位；
//   - 题集 = 前 sampleN 条 anchor（确定性抽样：文件顺序即样本顺序，
//     同样的样本号给同样的题集；需要随机样本时先 shuffle 文件）；
//   - 指纹 = 语料/题集内容的 sha256（内容寻址，两次装载同一批数据
//     得到同一指纹，可跨进程对比）。
//
// 样本号小于 0 = 全量。
func LoadCNLaw(path string, sampleN int) (*CNLawSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("evaldata: open %s: %w", path, err)
	}
	defer f.Close()

	seen := make(map[string]bool)
	var docs []retrieval.Document
	var items []evalfcore.Item
	itemIdx := 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20) // 单行可能很长
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec CNLawRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("evaldata: line %d: %w", itemIdx+1, err)
		}
		anchor := strings.TrimSpace(rec.Anchor)
		positive := strings.TrimSpace(rec.Positive)
		if anchor == "" || positive == "" {
			continue
		}
		id := hash12(positive)
		if !seen[id] {
			seen[id] = true
			docs = append(docs, retrieval.Document{ID: id, Body: positive})
		}
		if sampleN >= 0 && itemIdx >= sampleN {
			itemIdx++
			continue
		}
		items = append(items, evalfcore.Item{
			ID:       "q" + strconv.Itoa(itemIdx),
			Question: anchor,
			// 规则臂的“金标答案”取法条标题（短）——本数据集上规则臂不是
			// 记录指标（整段法条当答案规则臂恒 0，协议边界），记录指标是
			// 证据命中。标题只让规则臂有个合法口径，不当真。
			Answer:  titleOf(positive),
			GoldIDs: []string{id},
		})
		itemIdx++
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("evaldata: scan: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("evaldata: no items from %s", path)
	}
	return &CNLawSet{
		Docs:      docs,
		Items:     items,
		CorpusSHA: hashDocs(docs),
		ItemsSHA:  hashItems(items),
	}, nil
}

// titleOf 从 "title: … | text: …" 里取标题段。
func titleOf(positive string) string {
	const marker = " | text:"
	if i := strings.Index(positive, marker); i > 0 {
		t := strings.TrimPrefix(positive[:i], "title: ")
		return strings.TrimSpace(t)
	}
	r := []rune(positive)
	if len(r) > 80 {
		return string(r[:80])
	}
	return positive
}

func hash12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func hashDocs(docs []retrieval.Document) string {
	h := sha256.New()
	for _, d := range docs { // docs 已按文件首现顺序，确定性
		fmt.Fprintf(h, "%s\x00%s\x00", d.ID, d.Body)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func hashItems(items []evalfcore.Item) string {
	h := sha256.New()
	for _, it := range items {
		fmt.Fprintf(h, "%s\x00%s\x00", it.ID, it.Question)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// SortDocsForStableIndex 让索引构建顺序与文档集大小无关（大样本时用；
// 小样本可跳过）。导出供调用方在语料子集化后稳定化。
func SortDocsForStableIndex(docs []retrieval.Document) {
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
}
