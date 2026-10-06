package learncore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/willove/cumulus/internal/store"
)

// KVCycleStore 把周期档案落进 store 的 KV：一个周期一个 key = 原子。
type KVCycleStore struct {
	port store.Port
}

func NewKVCycleStore(port store.Port) *KVCycleStore { return &KVCycleStore{port: port} }

func cycleKey(id string) string { return "learn/cycle/" + id }

func (k *KVCycleStore) SaveCycle(ctx context.Context, rec CycleRecord) error {
	buf, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("learncore: encode cycle %s: %w", rec.ID, err)
	}
	if err := k.port.PutValue(ctx, cycleKey(rec.ID), buf); err != nil {
		return fmt.Errorf("learncore: save cycle %s: %w", rec.ID, err)
	}
	return nil
}

func (k *KVCycleStore) LoadCycle(ctx context.Context, id string) (CycleRecord, error) {
	buf, err := k.port.GetValue(ctx, cycleKey(id))
	if err != nil {
		return CycleRecord{}, fmt.Errorf("learncore: load cycle %s: %w", id, ErrCycleNotFound)
	}
	var rec CycleRecord
	if err := json.Unmarshal(buf, &rec); err != nil {
		return CycleRecord{}, fmt.Errorf("learncore: decode cycle %s: %w", id, err)
	}
	return rec, nil
}

var _ CycleStore = (*KVCycleStore)(nil)
