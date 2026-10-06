package evalfcore

import (
	"context"
	"fmt"

	"github.com/willove/cumulus/internal/store"
)

// KVStore 把 RunState 存进 store 的 KV：一个 run 一个 key = 原子写。
// 运行状态与逐题结果在同一个值里，被 kill 也不会落成半份
// （流程文法 §三的原子落盘，落点是这一行）。
type KVStore struct {
	port store.Port
}

// NewKVStore 用存储端口构造运行档案。
func NewKVStore(port store.Port) *KVStore { return &KVStore{port: port} }

func runKey(runID string) string { return "eval/run/" + runID }

func (k *KVStore) SaveRun(ctx context.Context, s RunState) error {
	buf, err := EncodeState(s)
	if err != nil {
		return err
	}
	if err := k.port.PutValue(ctx, runKey(s.RunID), buf); err != nil {
		return fmt.Errorf("evalfcore: save run %s: %w", s.RunID, err)
	}
	return nil
}

func (k *KVStore) LoadRun(ctx context.Context, runID string) (RunState, error) {
	buf, err := k.port.GetValue(ctx, runKey(runID))
	if err != nil {
		return RunState{}, fmt.Errorf("evalfcore: load run %s: %w", runID, ErrRunNotFound)
	}
	state, err := DecodeState(buf)
	if err != nil {
		return RunState{}, err
	}
	return state, nil
}

var _ RunStore = (*KVStore)(nil)
