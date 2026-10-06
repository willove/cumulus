// Package evalfcore 是流程二：一次评测运行（episode）。
//
// 协议从 cumulus 评测工作台 v2 迁移，形状按流程文法 §三 重定：
// 冻结（三指纹）、隔离（executor 每次独立）、登记化（判官/臂可换）、
// 原子落盘（状态与逐题结果同一个 KV 值）、可比性（指纹不全同只给理由）。
//
// 两条防泄漏纪律落在这里：
//   - executor 拿不到金标（只收问题），金标由 runner 持有——闭卷与判官
//     都无法从执行面看到答案；
//   - 判官未判分是 N/A（nil），不是 0。
package evalfcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Item 是评测的一条题。Answer 是短参考答案（规则臂按子串/数值边界匹配）；
// GoldIDs 是证据命中的金标文档 id（快照内精确 id，陈旧修订与同名业务键
// 都不给分——cumulus 的口径）。
type Item struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Answer   string   `json:"answer"`
	GoldIDs  []string `json:"gold_ids,omitempty"`
}

// ValidateItems 返回诊断列表：重复题号、空字段、超长金标。
// 超长金标（>200 字）只警告不拒绝——LENS 式整段引用会被规则臂恒判 0，
// 那是协议不匹配，不是检索失败，必须在跑之前让人看见。
func ValidateItems(items []Item) []string {
	var out []string
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if it.ID == "" {
			out = append(out, "item with empty id")
			continue
		}
		if seen[it.ID] {
			out = append(out, fmt.Sprintf("duplicate item id %q", it.ID))
		}
		seen[it.ID] = true
		if strings.TrimSpace(it.Question) == "" {
			out = append(out, fmt.Sprintf("item %s: empty question", it.ID))
		}
		if strings.TrimSpace(it.Answer) == "" {
			out = append(out, fmt.Sprintf("item %s: empty answer", it.ID))
		}
		if len([]rune(it.Answer)) > 200 {
			out = append(out, fmt.Sprintf("item %s: answer is %d runes (>200); rule arm will score 0 by protocol", it.ID, len([]rune(it.Answer))))
		}
	}
	return out
}

// Dataset 是不可变题集。同内容 => 同 ItemsSHA => 同 ID：保存两次同一份
// 内容得到同一个 id（内容寻址），改一个字节就是新题集。
type Dataset struct {
	ID       string `json:"id"`
	ItemsSHA string `json:"items_sha"`
	Items    []Item `json:"items"`
}

// NewDataset 校验并计算指纹。诊断非空时返回 error——坏题集不许进运行。
func NewDataset(items []Item) (Dataset, error) {
	if len(items) == 0 {
		return Dataset{}, fmt.Errorf("evalfcore: empty dataset")
	}
	if diags := ValidateItems(items); len(diags) > 0 {
		return Dataset{}, fmt.Errorf("evalfcore: dataset invalid: %s", strings.Join(diags, "; "))
	}
	// 排序后哈希：题序不同不算新题集（同一批题的两个顺序是同一内容）
	sorted := make([]Item, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	buf, err := json.Marshal(sorted)
	if err != nil {
		return Dataset{}, fmt.Errorf("evalfcore: marshal items: %w", err)
	}
	sum := sha256.Sum256(buf)
	sha := hex.EncodeToString(sum[:])
	return Dataset{ID: sha, ItemsSHA: sha, Items: sorted}, nil
}
