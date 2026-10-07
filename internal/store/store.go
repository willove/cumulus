// Package store 是存储端口：本包定义 cumulus 需要的存储能力，
// cumulite 适配器实现它。业务代码只认 Port，不认 cumulite——
// 换存储是换适配器，不动流程（契约在端口，不在依赖）。
//
// 当前唯一实现是 cumulite（线上版 github.com/willove/cumulite）。
package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
)

// Port 是 cumulus 用到的那部分存储能力。刻意保持小：
// 加方法进来要有对应的流程需求，不是“引擎有什么就暴露什么”。
type Port interface {
	// EnsureCollection 声明集合（幂等）。
	EnsureCollection(ctx context.Context, coll string) error
	// PutStruct 写入或替换一个结构体文档（upsert）。
	PutStruct(ctx context.Context, coll, id string, doc any) error
	// GetStruct 读出一个结构体文档。不存在返回 error（errors.Is(err, cumulite.ErrNotFound)）。
	GetStruct(ctx context.Context, coll, id string, out any) error
	// Delete 删除一个文档。不存在也返回 nil（幂等删除）。
	Delete(ctx context.Context, coll, id string) error
	// PutValue 写一段二进制值（KV）。**值要小**：评测档案的清单、学习
	// 周期的记录走这里；逐题结果这种随题数增长的内容走文档集合
	// （见 Query）。cumulite 的 KV 由 Badger 兜底，而 Badger 在
	// in-memory 模式下单值上限是 maxValueThreshold = 1 MiB（没有 value
	// log 可外溢）——整份档案塞一个值必然在题数上线性撞墙，实测 449 题
	// × 9 窗 = 1.05 MB 就炸。内容分开存是引擎的模态划分，不是绕限。
	PutValue(ctx context.Context, key string, value []byte) error
	// GetValue 读回二进制值。不存在返回 error（errors.Is(err, cumulite.ErrNotFound)）。
	GetValue(ctx context.Context, key string) ([]byte, error)
	// Query 按等值过滤（字段间 AND）分页读结构体文档，稳定按文档键序
	// ——顺序稳定是分页成立的前提（无稳定序的分页会重复与漏读）。
	// 返回本页条数；调用方用它判断"还有没有下一页"。
	Query(ctx context.Context, coll string, filter map[string]any, skip, limit int, out any) (int, error)
	// Health 探活。
	Health(ctx context.Context) error
	// ListIDs 枚举集合内全部文档 id（分页）。语料面需要它：索引是语料
	// 的投影，进程重启后从 store 重建，没有枚举语料就只能在导入进程的
	// 内存里活着。limit<=0 用引擎默认上限。
	ListIDs(ctx context.Context, coll string, limit int) ([]string, error)
}

// Open 打开存储。dir 为空且 inMemory 为真时用内存引擎（测试与 selftest 用）。
func Open(dir string, inMemory bool) (Port, error) {
	var opts []cumulite.Option
	if inMemory {
		opts = append(opts, cumulite.WithInMemory())
	}
	e, err := cumulite.Open(dir, opts...)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	return &adapter{engine: e}, nil
}

type adapter struct {
	engine *cumulite.Engine
}

func (a *adapter) EnsureCollection(ctx context.Context, coll string) error {
	if err := a.engine.EnsureCollection(ctx, coll); err != nil {
		return fmt.Errorf("store: ensure %s: %w", coll, err)
	}
	return nil
}

// PutStruct 走“先替换、不存在则插入”的 upsert。cumulite 的
// ReplaceStruct 对缺失文档报 not-found；插入必须经 Insert 并显式带 _id——
// InsertStructs 只管结构体、id 由引擎生成，拿不到我们手里的 id。
// 并发同名写入有小窗口（替换失败后、插入前被别人插了），由引擎键唯一性
// 报错兜底，调用方重试即可；单进程内这条路不会发生。
func (a *adapter) PutStruct(ctx context.Context, coll, id string, doc any) error {
	if _, err := a.engine.ReplaceStruct(ctx, coll, id, doc); err == nil {
		return nil
	} else if !cumulite.IsNotFound(err) {
		return fmt.Errorf("store: replace %s/%s: %w", coll, id, err)
	}
	buf, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("store: marshal %s/%s: %w", coll, id, err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		return fmt.Errorf("store: remap %s/%s: %w", coll, id, err)
	}
	m["_id"] = id
	if _, err := a.engine.Insert(ctx, coll, []map[string]any{m}); err != nil {
		return fmt.Errorf("store: insert %s/%s: %w", coll, id, err)
	}
	return nil
}

// GetStruct 用 JSON 往返把文档 map 转成调用方的结构体。
// 存储里存的是 wire 形态，结构体是进程内形态，转换发生在端口边界。
func (a *adapter) GetStruct(ctx context.Context, coll, id string, out any) error {
	raw, err := a.engine.GetDocument(ctx, coll, id)
	if err != nil {
		return fmt.Errorf("store: get %s/%s: %w", coll, id, err)
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("store: marshal %s/%s: %w", coll, id, err)
	}
	if err := json.Unmarshal(buf, out); err != nil {
		return fmt.Errorf("store: decode %s/%s: %w", coll, id, err)
	}
	return nil
}

func (a *adapter) Delete(ctx context.Context, coll, id string) error {
	if _, err := a.engine.DeleteDocument(ctx, coll, id); err != nil && !cumulite.IsNotFound(err) {
		return fmt.Errorf("store: delete %s/%s: %w", coll, id, err)
	}
	return nil
}

func (a *adapter) Health(ctx context.Context) error {
	if _, err := a.engine.Health(ctx); err != nil {
		return fmt.Errorf("store: health: %w", err)
	}
	return nil
}

// PutValue / GetValue 走引擎 KV。TTL 传 0（不过期）：评测运行与题集
// 是档案，不是缓存。
func (a *adapter) PutValue(ctx context.Context, key string, value []byte) error {
	if err := a.engine.KVPut(ctx, key, value, 0); err != nil {
		return fmt.Errorf("store: put value %s: %w", key, err)
	}
	return nil
}

func (a *adapter) GetValue(ctx context.Context, key string) ([]byte, error) {
	v, err := a.engine.KVGet(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("store: get value %s: %w", key, err)
	}
	return v, nil
}

// ListIDs 枚举集合内全部文档 id（分页）。语料面需要它：索引是语料的
// 投影，进程重启后从 store 重建——没有枚举，语料就只能在导入进程的
// 内存里活着。实现走引擎 Query 的 match-all（Filter 空 = 全量扫）。
func (a *adapter) ListIDs(ctx context.Context, coll string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100000
	}
	res, err := a.engine.Query(ctx, coll, contract.Query{Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("store: query %s: %w", coll, err)
	}
	ids := make([]string, 0, len(res.Documents))
	for _, d := range res.Documents {
		if id, ok := d["_id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Query 是等值过滤 + 分页的文档读。做的是存储形态（wire map）到进程内
// 形态（调用方结构体）的转换，与 GetStruct 同一条边界纪律。
func (a *adapter) Query(ctx context.Context, coll string, filter map[string]any, skip, limit int, out any) (int, error) {
	res, err := a.engine.Query(ctx, coll, contract.Query{Filter: filter, Skip: skip, Limit: limit})
	if err != nil {
		return 0, fmt.Errorf("store: query %s: %w", coll, err)
	}
	buf, err := json.Marshal(res.Documents)
	if err != nil {
		return 0, fmt.Errorf("store: marshal query page %s: %w", coll, err)
	}
	if err := json.Unmarshal(buf, out); err != nil {
		return 0, fmt.Errorf("store: decode query page %s: %w", coll, err)
	}
	return len(res.Documents), nil
}
