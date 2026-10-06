// Package learncore 是流程三：一次学习周期（受管变更五阶段）。
//
// 纪律（流程文法 §四）：只改 harness 运行条件，不改模型权重、业务语料、
// 评测协议。五阶段：观察 → 诊断 → 提议 → 评估 → 提升。
//   - 提议只准碰白名单旋钮（registry 把守，LLM 提议也越界不了）；
//   - 评估不过护栏不上线；无护栏基线拒跑；
//   - 提升 = 注册：旧值当场存下，掉线即回滚；
//   - 全程落盘。
package learncore

import (
	"fmt"
	"sort"
)

// Knob 是一枚旋钮。旋钮是声明出来的，不是顺手加的。
type Knob struct {
	Name  string
	Min   float64
	Max   float64
	value float64
}

// NewKnob 造一枚旋钮，初始值必须在界内。
func NewKnob(name string, min, max, initial float64) (*Knob, error) {
	if min >= max {
		return nil, fmt.Errorf("learncore: knob %s: min must be < max", name)
	}
	if initial < min || initial > max {
		return nil, fmt.Errorf("learncore: knob %s: initial %v outside [%v,%v]", name, initial, min, max)
	}
	return &Knob{Name: name, Min: min, Max: max, value: initial}, nil
}

func (k *Knob) Value() float64 { return k.value }

// Set 改值。越界拒绝——旋钮的界就是它的全部合法性。
func (k *Knob) Set(v float64) error {
	if v < k.Min || v > k.Max {
		return fmt.Errorf("learncore: knob %s: %v outside [%v,%v]", k.Name, v, k.Min, k.Max)
	}
	k.value = v
	return nil
}

// Registry 是白名单。不在表里的名字一律拒绝。
type Registry struct {
	knobs map[string]*Knob
}

func NewRegistry(ks ...*Knob) *Registry {
	r := &Registry{knobs: make(map[string]*Knob, len(ks))}
	for _, k := range ks {
		r.knobs[k.Name] = k
	}
	return r
}

func (r *Registry) Get(name string) (*Knob, bool) {
	k, ok := r.knobs[name]
	return k, ok
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.knobs))
	for n := range r.knobs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Snapshot 导出现值（回滚与落盘用）。
func (r *Registry) Snapshot() map[string]float64 {
	out := make(map[string]float64, len(r.knobs))
	for n, k := range r.knobs {
		out[n] = k.value
	}
	return out
}

// Restore 整体回滚到一套旧值。越界或未知名字都报错——回滚不许静默失肤。
func (r *Registry) Restore(m map[string]float64) error {
	for name, v := range m {
		k, ok := r.knobs[name]
		if !ok {
			return fmt.Errorf("learncore: rollback: unknown knob %q", name)
		}
		if err := k.Set(v); err != nil {
			return fmt.Errorf("learncore: rollback: %w", err)
		}
	}
	return nil
}
