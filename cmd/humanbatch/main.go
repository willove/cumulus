// humanbatch 从一次评测档案里导出**人工标注批次**，并按"两个验证器是否一致"分层。
//
// 它存在的理由不是"再一个工具"，而是两条纪律的机械保证：
//  1. **盲标**：批次里不带信号值、不带任一验证器的判断（那两个判断挪到
//     meta 字段里，标注时折叠不看）——锚定效应是这类标注最大的污染源；
//  2. **标签要能防伪**：批次是内容寻址的（items 指纹 + 抽样参数），
//     同一份题集 + 同一份抽样 → 同一个批次 id；换抽样 = 换批次。
//
// 用法：
//
//	humanbatch -store <dir> -run <runID> -data <数据集目录> -out <file.jsonl> [-mode disagree]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/willove/cumulus/internal/evaldata"
	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/store"
)

// batchRow 是一条待标注题（批次文件的一行）。
// batchMeta 是批次元信息（机器判断，标注时折叠）。
type batchMeta struct {
	JudgeOK    *bool   `json:"judge_ok,omitempty"`
	JudgeCov   float64 `json:"judge_coverage,omitempty"`
	DecideNoul float64 `json:"decide_noul,omitempty"`
	Evidence   bool    `json:"evidence_hit"`
	Agree      bool    `json:"verify_agree"`
	Layer      string  `json:"layer"`
}

type batchRow struct {
	ID       string    `json:"id"`
	Question string    `json:"question"`
	Answer   string    `json:"answer"`
	Windows  []string  `json:"windows"`
	Label    string    `json:"label"`
	Hits     []int     `json:"hit_points,omitempty"`
	Note     string    `json:"note,omitempty"`
	Meta     batchMeta `json:"meta"`
}

func main() {
	var storeDir, runID, dataDir, out string
	var mode, mdOut, applyFrom string
	flag.StringVar(&storeDir, "store", "", "CUMULUS_STORE_DIR 落盘目录")
	flag.StringVar(&runID, "run", "run-selftest", "run id")
	flag.StringVar(&dataDir, "data", "", "数据集目录（corpus.jsonl + items.jsonl）")
	flag.StringVar(&out, "out", "", "输出 JSONL")
	flag.StringVar(&mode, "mode", "disagree", "disagree（只看两个验证器不一致的）| all（全部）")
	flag.StringVar(&mdOut, "md", "", "同时渲染一份 Markdown（盲标：机器判断放附录）")
	flag.StringVar(&applyFrom, "apply", "", "从标注好的 Markdown 回填 label 到 -out")
	flag.Parse()

	if applyFrom != "" {
		if out == "" {
			fmt.Fprintln(os.Stderr, "-apply 需要配 -out（回填后的 JSONL 路径）")
			os.Exit(2)
		}
		if err := applyLabels(applyFrom, out); err != nil {
			fail(err)
		}
		return
	}
	if storeDir == "" || dataDir == "" || out == "" {
		fmt.Fprintln(os.Stderr, "用法：humanbatch -store <dir> -data <数据集目录> -out <file.jsonl> [-mode disagree|all]")
		os.Exit(2)
	}

	ctx := context.Background()
	st, err := store.Open(storeDir, false)
	if err != nil {
		fail(err)
	}
	state, err := evalfcore.NewArchive(st).LoadRun(ctx, runID)
	if err != nil {
		fail(err)
	}
	set, _, err := evaldata.LoadJSONL(dataDir+"/corpus.jsonl", dataDir+"/items.jsonl")
	if err != nil {
		fail(err)
	}
	idx := retrieval.Build(set.Docs)
	questions := map[string]string{}
	golds := map[string][]string{}
	for _, it := range set.Items {
		questions[it.ID] = it.Question
		golds[it.ID] = it.GoldIDs
	}

	var rows []batchRow
	if mode == "balanced" {
		// 分层抽样：**结论要有方差**。真跑踩过——只按"两验证器分歧"抽，
		// 结果人工几乎全判 YES，「恒答 YES」基线就有 96%，这批数据区分不了
		// 任何验证器。按（决策模型 noul 三档 × 证据命中）分层各取几道，
		// 才可能把"信号能不能挑出坏答案"这个问题问出答案。
		buckets := map[string][]evalfcore.ItemResult{}
		for _, r := range state.Results {
			buckets[stratum(r)] = append(buckets[stratum(r)], r)
		}
		keys := make([]string, 0, len(buckets))
		for k := range buckets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		per := 5
		if v := os.Getenv("HUMAN_BATCH_PER_STRATUM"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 20 {
				per = n
			}
		}
		for _, k := range keys {
			items := buckets[k]
			for i, r := range items {
				if i >= per {
					break
				}
				w := batchRow{ID: r.ItemID}
				w.Meta = metaOf(r)
				w.Meta.Layer = k
				rows = append(rows, w)
			}
		}
	}
	for _, r := range state.Results {
		if mode == "balanced" {
			break
		}
		agree := false
		if r.JudgeOK != nil {
			agree = (r.VerifyNoul >= 0.5) == *r.JudgeOK
		}
		if mode == "disagree" && r.JudgeOK != nil && agree {
			continue
		}
		w := batchRow{ID: r.ItemID, Question: questions[r.ItemID], Answer: r.Answer}
		for _, span := range r.CitedSpans {
			parts := strings.SplitN(span, "#", 2)
			if len(parts) != 2 {
				continue
			}
			if d, ok := idx.Doc(parts[0]); ok {
				if text, err := retrieval.ResolveSpan(d.Body, parts[1]); err == nil {
					w.Windows = append(w.Windows, truncate(text, 240))
				}
			}
		}
		w.Meta.JudgeOK = r.JudgeOK
		w.Meta.JudgeCov = r.JudgeCoverage
		w.Meta.DecideNoul = r.VerifyNoul
		w.Meta.Evidence = r.EvidenceHit
		w.Meta.Agree = agree
		w.Meta.Layer = layerOf(r)
		rows = append(rows, w)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	f, err := os.Create(out)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	for _, w := range rows {
		buf, err := json.Marshal(w)
		if err != nil {
			fail(err)
		}
		fmt.Fprintln(f, string(buf))
	}
	fmt.Printf("导出 %d 题 → %s（items=%s run=%s mode=%s）\n", len(rows), out, state.Fingerprints.ItemsSHA[:12], runID, mode)
	// 分层统计（决定人工标多少、标哪层）
	counts := map[string]int{}
	for _, w := range rows {
		counts[w.Meta.Layer]++
	}
	fmt.Println("分层：", counts)

	if mdOut != "" {
		if err := renderMarkdown(mdOut, rows, state.Fingerprints.ItemsSHA); err != nil {
			fail(err)
		}
		fmt.Printf("Markdown → %s\n", mdOut)
	}
}

// renderMarkdown 渲染成"人读得下去"的标注稿。
//
// **盲标是硬要求**：机器判断（判官/决策模型）一律不进正文，只放附录——
// 标注者填完上面才看得到，否则就是拿着答案猜。附录的存在也让"填完再对答案"
// 成为可能。
func renderMarkdown(path string, rows []batchRow, itemsSHA string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(f, "# 人工标注批次（盲标）\n\n")
	fmt.Fprintf(f, "- 题数：%d · 题集指纹：`%s` · 抽样：两个验证器判断不一致 + 边界 + 随机基准\n",
		len(rows), itemsSHA)
	fmt.Fprintf(f, "- **两问分开标**（第一问只需要读证据；第二问才需要你的世界知识）：\n")
	fmt.Fprintf(f, "  1. **接地**：答案的事实点能否在上面的**证据窗口原文**里找到依据？\n")
	fmt.Fprintf(f, "     YES = 能（措辞不同没关系）· PARTIAL = 部分能且无矛盾 · NO = 有矛盾或完全无依据 · **UNK = 你判不了**\n")
	fmt.Fprintf(f, "  2. **证据充分性**：这些窗口本身够不够判断？（够 / 不够）\n")
	fmt.Fprintf(f, "- **UNK 是合法答案**：人的知识有边界（真跑踩过：把没有把握的题逼成 YES/NO，")
	fmt.Fprintf(f, "标签就变成了噪声）。判不了就写 UNK，我们会把它排除出准确率而不是当成错。\n")
	fmt.Fprintf(f, "- 每题只需在 `**标注**：` 后写 YES / PARTIAL / NO；`备注` 可留空。\n")
	fmt.Fprintf(f, "- 附录里的机器判断**填完再翻**（它就是待评估的对象）。\n\n")
	fmt.Fprintf(f, "---\n\n")
	for i, r := range rows {
		fmt.Fprintf(f, "## %d. `%s`　层：`%s`\n\n", i+1, r.ID, r.Meta.Layer)
		fmt.Fprintf(f, "**问题**：%s\n\n", oneLine(r.Question))
		fmt.Fprintf(f, "**答案**：\n\n%s\n\n", blockquote(r.Answer))
		fmt.Fprintf(f, "**证据窗口**（%d 条）：\n\n", len(r.Windows))
		for j, w := range r.Windows {
			fmt.Fprintf(f, "%d. %s\n", j+1, oneLine(w))
		}
		fmt.Fprintf(f, "\n**接地标注**：__________　　**证据充分性**：__________　　**备注**：__________\n\n---\n\n")
	}
	fmt.Fprintf(f, "\n## 附：机器判断（填完上面再看）\n\n")
	fmt.Fprintf(f, "| # | id | 分点判官 | 判官覆盖率 | 决策模型 noul | 证据命中 | 两验证器一致 |\n")
	fmt.Fprintf(f, "|---|---|---|---|---|---|---|\n")
	for i, r := range rows {
		judge := "—"
		if r.Meta.JudgeOK != nil {
			if *r.Meta.JudgeOK {
				judge = "YES"
			} else {
				judge = "NO"
			}
		}
		fmt.Fprintf(f, "| %d | `%s` | %s | %.2f | %.2f | %v | %v |\n",
			i+1, r.ID, judge, r.Meta.JudgeCov, r.Meta.DecideNoul, r.Meta.Evidence, r.Meta.Agree)
	}
	return nil
}

// applyLabels 从标注好的 Markdown 回填 label（按题号顺序配对）。
func applyLabels(mdPath, outPath string) error {
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		return err
	}
	labels := map[string]string{}
	var ids []string
	lines := strings.Split(string(raw), "\n")
	cur := ""
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") && strings.Contains(ln, "`") {
			if i := strings.Index(ln, "`"); i >= 0 {
				rest := ln[i+1:]
				if j := strings.Index(rest, "`"); j >= 0 {
					cur = rest[:j]
					ids = append(ids, cur)
				}
			}
			continue
		}
		if cur == "" {
			continue
		}
		if strings.HasPrefix(ln, "**接地标注**") {
			v := ln
			if i := strings.Index(v, "**接地标注**："); i >= 0 {
				v = v[i+len("**接地标注**："):]
			}
			if k := strings.Index(v, "　"); k > 0 {
				v = v[:k]
			}
			v = strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(v), "_", ""))
			switch strings.ToUpper(v) {
			case "YES", "PARTIAL", "NO", "UNK":
				labels[cur] = strings.ToUpper(v)
			}
		}
	}
	src := os.Getenv("HUMAN_BATCH_SRC")
	if src == "" {
		return fmt.Errorf("需要 HUMAN_BATCH_SRC 指向原批次 JSONL（回填要往里写 label）")
	}
	rows, err := readBatch(src)
	if err != nil {
		return err
	}
	n := 0
	for i := range rows {
		if l, ok := labels[rows[i].ID]; ok {
			rows[i].Label = l
			n++
		}
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, r := range rows {
		buf, err := json.Marshal(r)
		if err != nil {
			return err
		}
		fmt.Fprintln(f, string(buf))
	}
	fmt.Printf("回填 %d/%d 题 → %s\n", n, len(rows), outPath)
	if n == 0 {
		fmt.Fprintln(os.Stderr, "没有解析到任何标注：检查 **标注**： 那行是否写了 YES/PARTIAL/NO")
	}
	return nil
}

func readBatch(path string) ([]batchRow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []batchRow
	for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var r batchRow
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// blockquote 把答案渲染成 Markdown 引用块（调用方不要再加 ">"）。
func blockquote(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == "" {
			out = append(out, ">")
			continue
		}
		out = append(out, "> "+strings.TrimRight(l, " "))
	}
	return strings.Join(out, "\n")
}

// stratum 是分层键：决策模型 noul 三档 × 证据命中。
// 分层抽样的目的不是"覆盖难例"，而是**让结论有方差**。
func stratum(r evalfcore.ItemResult) string {
	b := "mid"
	switch {
	case r.VerifyNoul < 0.5:
		b = "low"
	case r.VerifyNoul >= 0.85:
		b = "high"
	}
	hit := "miss"
	if r.EvidenceHit {
		hit = "hit"
	}
	return b + "/" + hit
}

// metaOf 把一条结果折成批次元信息（不含 label）。
func metaOf(r evalfcore.ItemResult) (m batchMeta) {
	m.JudgeOK = r.JudgeOK
	m.JudgeCov = r.JudgeCoverage
	m.DecideNoul = r.VerifyNoul
	m.Evidence = r.EvidenceHit
	if r.JudgeOK != nil {
		m.Agree = (r.VerifyNoul >= 0.5) == *r.JudgeOK
	}
	return m
}

// layerOf 决定这题值不值得人看：分歧 > 低分边界 > 随机。
func layerOf(r evalfcore.ItemResult) string {
	if r.JudgeOK != nil && (r.VerifyNoul >= 0.5) != *r.JudgeOK {
		return "disagree"
	}
	if r.VerifyNoul > 0 && r.VerifyNoul < 0.5 {
		return "boundary"
	}
	return "random"
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "humanbatch:", err)
	os.Exit(1)
}
