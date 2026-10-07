package evaldata

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/retrieval"
)

// 本地 JSONL 数据集：把任何来源裁剪好的小语料接进评测。
//
// 为什么要这个入口：cn-law-rag 是写死的装载函数，外部数据集（ModelScope
// 拉的 CMRC、自己裁的领域小语料、将来按块切的校准集）不该每来一个就改一次
// eval.go。统一形状两条文件：
//
//	corpus.jsonl  {"id","title","body"}                    —— 检索宇宙
//	items.jsonl   {"id","question","answer","gold_ids"}    —— 题集（金标 = 文档 id）
//
// 两条都内容寻址算指纹（同内容同指纹），于是"哪份语料 + 哪份题集"可跨进程
// 比对——校准集与锁箱的划分靠的就是这个（同语料、不同题集块 = 不同 ItemsSHA，
// 语料指纹相同）。
//
// 裁剪脚本见 scripts/prep_cmrc.py（ModelScope → 小语料）。
type LocalSet struct {
	Docs      []retrieval.Document
	Items     []evalfcore.Item
	CorpusSHA string
	ItemsSHA  string
}

// LocalCorpusLine / LocalItemLine 是两份 JSONL 的行形状。
type LocalCorpusLine struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

type LocalItemLine struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Answer   string   `json:"answer"`
	GoldIDs  []string `json:"gold_ids"`
}

// LoadJSONL 读语料与题集两条 JSONL。
//   - 语料行的 id 为空时按正文内容哈希补（内容寻址：同内容同 id）；
//   - 题集行的 Question 为空即拒（空问题不是题）；
//   - 金标 id 不在语料里**只警告不拒**：那是"该拒答的题"，是一种合法
//     评测构造（召回失败的对照组），但它必须是显式选择，所以警告要看得见。
func LoadJSONL(corpusPath, itemsPath string) (*LocalSet, []string, error) {
	corpus, err := os.Open(corpusPath)
	if err != nil {
		return nil, nil, fmt.Errorf("evaldata: open corpus: %w", err)
	}
	defer corpus.Close()

	var docs []retrieval.Document
	seen := map[string]bool{}
	sc := bufio.NewScanner(corpus)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var cl LocalCorpusLine
		if err := json.Unmarshal([]byte(line), &cl); err != nil {
			return nil, nil, fmt.Errorf("evaldata: corpus line: %w", err)
		}
		body := strings.TrimSpace(cl.Body)
		if body == "" {
			continue
		}
		id := cl.ID
		if id == "" {
			id = hash12(body)
		}
		if seen[id] {
			continue // 内容寻址天然去重
		}
		seen[id] = true
		docs = append(docs, retrieval.Document{ID: id, Body: body})
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("evaldata: corpus scan: %w", err)
	}
	if len(docs) == 0 {
		return nil, nil, fmt.Errorf("evaldata: no documents in %s", corpusPath)
	}

	itemsFile, err := os.Open(itemsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("evaldata: open items: %w", err)
	}
	defer itemsFile.Close()

	var items []evalfcore.Item
	sc = bufio.NewScanner(itemsFile)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var il LocalItemLine
		if err := json.Unmarshal([]byte(line), &il); err != nil {
			return nil, nil, fmt.Errorf("evaldata: items line: %w", err)
		}
		if strings.TrimSpace(il.Question) == "" {
			return nil, nil, fmt.Errorf("evaldata: item %q has empty question", il.ID)
		}
		id := il.ID
		if id == "" {
			id = hash12(il.Question)
		}
		items = append(items, evalfcore.Item{
			ID:       id,
			Question: il.Question,
			Answer:   il.Answer,
			GoldIDs:  il.GoldIDs,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("evaldata: items scan: %w", err)
	}
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("evaldata: no items in %s", itemsPath)
	}

	var warnings []string
	for _, it := range items {
		if len(it.GoldIDs) == 0 {
			warnings = append(warnings, fmt.Sprintf("item %s: no gold ids (recall-miss control group?)", it.ID))
			continue
		}
		for _, g := range it.GoldIDs {
			if !seen[g] {
				warnings = append(warnings, fmt.Sprintf("item %s: gold id %s not in corpus (unanswerable by construction)", it.ID, g))
			}
		}
	}
	if missing := len(items) - countAnswerable(items, seen); missing > 0 {
		warnings = append(warnings, fmt.Sprintf("%d/%d items have no gold evidence in the corpus — they measure refusal, not recall", missing, len(items)))
	}
	sort.Strings(warnings)

	return &LocalSet{
		Docs:      docs,
		Items:     items,
		CorpusSHA: HashDocs(docs),
		ItemsSHA:  HashItems(items),
	}, warnings, nil
}

func countAnswerable(items []evalfcore.Item, inCorpus map[string]bool) int {
	n := 0
	for _, it := range items {
		for _, g := range it.GoldIDs {
			if inCorpus[g] {
				n++
				break
			}
		}
	}
	return n
}

// DefaultLocalPaths 是约定路径：一个目录里放 corpus.jsonl 与 items.jsonl。
func DefaultLocalPaths(dir string) (corpus, items string) {
	return filepath.Join(dir, "corpus.jsonl"), filepath.Join(dir, "items.jsonl")
}
