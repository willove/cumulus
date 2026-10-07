package evalfcore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/willove/cumulus/internal/store"
)

// Archive 是评测档案的**文档模态**实现。
//
// 形状（对齐 cumulite 的模态划分：文档=内容，KV=小值）：
//   - 逐题结果 → `eval_items` 集合里的一个文档，id = "<runID>/<9 位序号>"，
//     写完不再改（append-only）；
//   - 运行清单 → KV 一个小值 `eval/run/<runID>`，**最后写**。
//
// 为什么不是"整份档案一个 KV 值"：cumulite 的 KV 由 Badger 兜底，而
// Badger 在 in-memory 模式下单值上限是 maxValueThreshold = 1 MiB（内存
// 引擎没有 value log 可以外溢大值）。档案随题数线性增长，449 题 × 9 窗
// 实测 1.05 MB——撞墙是必然，不是边界。内容分开存是引擎的模态划分。
//
// 原子性口径没变，只是换了承载：**清单最后写** ⇒ 清单里的 items_done
// 永远不领先于已落盘的逐题结果；被 kill 后读到的还是上一次的完整前缀。
// 逐题文档的 id 由序号决定，重写同一 run 是幂等覆盖。
type Archive struct {
	port store.Port
}

// EvalItemsCollection 是逐题结果的集合名。
const EvalItemsCollection = "eval_items"

// pageSize 是一次 Query 的页大小（分页读，避免一次把全 run 拉进内存）。
const pageSize = 500

// NewArchive 用存储端口构造文档模态档案。
func NewArchive(port store.Port) *Archive { return &Archive{port: port} }

func (a *Archive) SaveRun(ctx context.Context, s RunState) error {
	if err := a.port.EnsureCollection(ctx, EvalItemsCollection); err != nil {
		return fmt.Errorf("evalfcore: ensure %s: %w", EvalItemsCollection, err)
	}
	// append-only：只写清单里还没有的那一段。Running 档案的续写与
	// "同一 run 再存一次"都遵守这条；已落盘的题不再重写（评测的题
	// 结果一旦产生就不该变——Start 对 done/failed 的 run 直接拒绝重跑）。
	written := 0
	if prev, err := a.loadManifest(ctx, s.RunID); err == nil && prev.Format == archiveFormat {
		written = prev.Items
	}
	if written > len(s.Results) {
		written = len(s.Results) // 理论上不会发生；真发生了宁可重写也不丢
	}
	for i := written; i < len(s.Results); i++ {
		doc := itemDoc{RunID: s.RunID, Seq: i, ItemResult: s.Results[i]}
		if err := a.port.PutStruct(ctx, EvalItemsCollection, itemID(s.RunID, i), doc); err != nil {
			return fmt.Errorf("evalfcore: save run %s item %d: %w", s.RunID, i, err)
		}
	}
	man := runManifest{
		Format:       archiveFormat,
		RunID:        s.RunID,
		Arm:          s.Arm,
		Collection:   EvalItemsCollection,
		Items:        len(s.Results),
		ItemsTotal:   s.ItemsTotal,
		ItemErrors:   s.ItemErrors,
		Status:       s.Status,
		Error:        s.Error,
		Fingerprints: s.Fingerprints,
	}
	mbuf, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("evalfcore: encode manifest %s: %w", s.RunID, err)
	}
	// 清单最后写：这一行是"进度不领先于已落盘结果"的全部依据。
	if err := a.port.PutValue(ctx, runKey(s.RunID), mbuf); err != nil {
		return fmt.Errorf("evalfcore: save run %s manifest: %w", s.RunID, err)
	}
	return nil
}

func (a *Archive) LoadRun(ctx context.Context, runID string) (RunState, error) {
	raw, err := a.port.GetValue(ctx, runKey(runID))
	if err != nil {
		return RunState{}, fmt.Errorf("evalfcore: load run %s: %w", runID, ErrRunNotFound)
	}
	// 旧档案兼容：单值 RunState 与字节分片（runparts）都还能读回来。
	if !isManifest(raw) {
		return DecodeState(raw)
	}
	if man, err := decodeManifest(raw); err == nil && man.Format == runPartsFormat {
		return a.loadParts(ctx, runID, man)
	}
	man, err := a.loadManifest(ctx, runID)
	if err != nil {
		return RunState{}, fmt.Errorf("evalfcore: load run %s manifest: %w", runID, err)
	}
	if man.Format != archiveFormat {
		return RunState{}, fmt.Errorf("evalfcore: run %s: unknown archive format %q", runID, man.Format)
	}

	state := RunState{
		RunID:        runID,
		Arm:          man.Arm,
		Status:       man.Status,
		Fingerprints: man.Fingerprints,
		ItemsTotal:   man.ItemsTotal,
		ItemErrors:   man.ItemErrors,
		Error:        man.Error,
	}
	// 分页读逐题结果（稳定键序，id 序号零填充 ⇒ 字典序 == 序号序）。
	//
	// **清单把关**：只读清单认可的前 man.Items 条。半程被 kill 时可能
	// 已经写进去一些题、清单还没更新（清单最后写）——那些条目不进这次
	// 读的结果，于是"读到的 run"与"清单说有几步完成"始终是一致的。
	// 清单是权威，集合里的多余条目留给下一次续写覆盖。
	for skip := 0; len(state.Results) < man.Items; skip += pageSize {
		want := man.Items - len(state.Results)
		limit := pageSize
		if want < limit {
			limit = want
		}
		var page []itemDoc
		n, err := a.port.Query(ctx, man.Collection, map[string]any{"run_id": runID}, skip, limit, &page)
		if err != nil {
			return RunState{}, fmt.Errorf("evalfcore: load run %s items: %w", runID, err)
		}
		for _, d := range page {
			if d.Seq != len(state.Results) {
				continue // 序号不连续：漏条也不乱序（如实少读，不假装完整）
			}
			state.Results = append(state.Results, d.ItemResult)
		}
		if n == 0 {
			break // 清单说 N 条但集合里不足：返回读到的部分，不伪造
		}
	}
	state.ItemsDone = len(state.Results)
	return state, nil
}

// itemDoc 是集合里的一个文档：外层是档案坐标（run_id/seq），内嵌逐题结果。
// 内嵌让 JSON 扁平（字段与 ItemResult 同层），过滤与反序列化都不用拆包。
type itemDoc struct {
	RunID string `json:"run_id"`
	Seq   int    `json:"seq"`
	ItemResult
}

// itemID 生成逐题文档 id：序号零填充 9 位，保证字典序 == 数字序
// （分页按文档键序走，序错了分页就会重复与漏读）。
func itemID(runID string, seq int) string {
	return fmt.Sprintf("%s/%09d", runID, seq)
}

func (a *Archive) loadManifest(ctx context.Context, runID string) (runManifest, error) {
	raw, err := a.port.GetValue(ctx, runKey(runID))
	if err != nil {
		return runManifest{}, err
	}
	return decodeManifest(raw)
}

// loadParts 是旧的字节分片档案的读路径（只读，不再写新档案）。
func (a *Archive) loadParts(ctx context.Context, runID string, man runManifest) (RunState, error) {
	whole := make([]byte, 0, man.Bytes)
	for i := 0; i < man.Parts; i++ {
		part, err := a.port.GetValue(ctx, partKey(runID, man.Generation, i))
		if err != nil {
			return RunState{}, fmt.Errorf("evalfcore: load run %s part %d/%d: %w", runID, i+1, man.Parts, err)
		}
		whole = append(whole, part...)
	}
	return DecodeState(whole)
}

// runKey 是清单键（沿用单值版的键名：旧档案原地可读）。
func runKey(runID string) string { return "eval/run/" + runID }

const (
	// archiveFormat 是文档模态档案的清单格式标记。
	archiveFormat = "rundocs/v1"
	// runPartsFormat 是旧的字节分片格式标记（只读兼容）。
	runPartsFormat = "runparts/v1"
)

// runManifest 是档案清单：够小，进一个 KV 值（几 KB 量级）。
type runManifest struct {
	Format       string       `json:"format"`
	RunID        string       `json:"run_id"`
	Arm          string       `json:"arm,omitempty"`
	Collection   string       `json:"collection"`
	Items        int          `json:"items"` // 已落盘的逐题结果条数
	ItemsTotal   int          `json:"items_total"`
	ItemErrors   int          `json:"item_errors,omitempty"`
	Status       Status       `json:"status"`
	Error        string       `json:"error,omitempty"`
	Fingerprints Fingerprints `json:"fingerprints"`
	// 旧字节分片档案的字段（只读兼容用）
	Generation int `json:"generation,omitempty"`
	Parts      int `json:"parts,omitempty"`
	Bytes      int `json:"bytes,omitempty"`
}

func decodeManifest(buf []byte) (runManifest, error) {
	var m runManifest
	if err := json.Unmarshal(buf, &m); err != nil {
		return runManifest{}, err
	}
	return m, nil
}

// isManifest 用格式标记区分清单与旧的单值 RunState。判据是**顶层
// format 字段非空**，不是子串匹配——子串会在内容里恰好出现这个词时误判，
// 而这里误判的代价是"把档案读成乱码"。
func isManifest(buf []byte) bool {
	var probe struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(buf, &probe); err != nil {
		return false
	}
	return probe.Format != ""
}

func partKey(runID string, gen, i int) string {
	return fmt.Sprintf("eval/run/%s/g%d/p%d", runID, gen, i)
}

var _ RunStore = (*Archive)(nil)
