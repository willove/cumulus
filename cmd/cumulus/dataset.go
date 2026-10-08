package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/willove/cumulus/internal/evaldata"
	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/retrieval"
)

// resolveDataset 装评测题集：**来源 → 切分 → 采样**，顺序不可调换。
//
// 为什么顺序是契约：
//   - **切分决定身份**（题 id 哈希，与顺序无关、加题不搬家）——锁箱纪律的前提；
//   - **采样只是成本旋钮**，作用在切分后的集合上（"跑 300 题"指这一份里的 300 题，
//     不是全库前 300 题）。
//
// 四条来源：内联小样 / cn-law（真实运行语料）/ 本地 JSONL 对（ModelScope 拉的
// CMRC/DuReader/DomainRAG 自裁）/ 无。
func resolveDataset() ([]retrieval.Document, []evalfcore.Item, string, string, error) {
	corpus := evalCorpus()
	items := evalItems()
	// 内联小样也给真指纹：占位串会在 [:12] 截断处炸（且"同样内容同指纹"的
	// 纪律对演示路径同样成立）。
	corpusSHA := evaldata.HashDocs(corpus)
	itemsSHA := evaldata.HashItems(items)

	if os.Getenv("CUMULUS_REALDATA") == "cnlaw" {
		path := os.Getenv("CNLAW_DIR")
		if path == "" {
			path = filepath.Join(os.Getenv("HOME"), "datasets/cn-law-rag/finetune_dataset.jsonl")
		}
		// -1 = 全量（0 的语义是"取 0 条"，真跑踩过）；统一采样交给 applySplitAndSample
		set, err := evaldata.LoadCNLaw(path, -1)
		if err != nil {
			return nil, nil, "", "", err
		}
		corpus, items = set.Docs, set.Items
		corpusSHA, itemsSHA = set.CorpusSHA, set.ItemsSHA
		fmt.Printf("realdata: %s corpus=%d docs\n", path, len(corpus))
	}

	// local：任意来源裁剪好的小语料（目录里放 corpus.jsonl + items.jsonl）
	dir := os.Getenv("CUMULUS_LOCAL_DIR")
	if dir != "" {
		corpusPath := filepath.Join(dir, "corpus.jsonl")
		itemsPath := filepath.Join(dir, "items.jsonl")
		if v := os.Getenv("CUMULUS_LOCAL_CORPUS"); v != "" {
			corpusPath = v
		}
		if v := os.Getenv("CUMULUS_LOCAL_ITEMS"); v != "" {
			itemsPath = v
		}
		set, warnings, err := evaldata.LoadJSONL(corpusPath, itemsPath)
		if err != nil {
			return nil, nil, "", "", err
		}
		corpus, items = set.Docs, set.Items
		corpusSHA, itemsSHA = set.CorpusSHA, set.ItemsSHA
		fmt.Printf("local: corpus=%s items=%s docs=%d\n", corpusPath, itemsPath, len(corpus))
		// 数据诊断必须看得见：金标不在语料里是"该拒答"的合法构造，但要显式
		// 选择，不能默默跑（否则把数据错当成检索失败）。
		for _, w := range warnings {
			fmt.Printf("  warn: %s\n", w)
		}
	}

	items, err := applySplitAndSample(items)
	if err != nil {
		return nil, nil, "", "", err
	}
	return corpus, items, corpusSHA, itemsSHA, nil
}
