package context

import (
	"errors"
	"testing"
)

// testComp 是一个可编排的可选组件：记录激活/停用次数。
type testComp struct {
	name      string
	requires  []string
	activates int
	deacts    int
	actErr    error
}

func (t *testComp) Name() string       { return t.name }
func (t *testComp) Requires() []string { return t.requires }
func (t *testComp) Activate(c *Context) error {
	if t.actErr != nil {
		return t.actErr
	}
	t.activates++
	return nil
}
func (t *testComp) Deactivate(c *Context) error { t.deacts++; return nil }

func keyFor(name string) Key[string] { return NewKey[string](name) }

// 依赖缺着：组件不激活，缺什么列出来。
func TestComponentInactiveUntilDepsBound(t *testing.T) {
	c := New("default")
	comp := &testComp{name: "dep-a", requires: []string{"a", "b"}}
	c.RegisterComponent(comp)
	if comp.activates != 0 {
		t.Fatal("must not activate with missing deps")
	}
	states := c.Components()
	if len(states) != 1 || states[0].Active {
		t.Fatalf("bad state: %+v", states)
	}
	if len(states[0].Missing) != 2 {
		t.Fatalf("both deps must be listed missing, got %v", states[0].Missing)
	}
}

// 绑齐 → 激活；解绑一个 → 停用。分类由 context 驱动，组件不猜。
func TestComponentActivatesOnBindAndDeactivatesOnUnwind(t *testing.T) {
	c := New("default")
	comp := &testComp{name: "dep-a", requires: []string{"a"}}
	c.RegisterComponent(comp)

	ka := keyFor("a")
	if err := Set(c, ka, "v"); err != nil {
		t.Fatal(err)
	}
	if comp.activates != 1 {
		t.Fatalf("bind must activate once, got %d", comp.activates)
	}
	if !c.Components()[0].Active {
		t.Fatal("status must show active")
	}

	mark := c.Mark()
	_ = Set(c, ka, "v2")
	if comp.activates != 1 || comp.deacts != 0 {
		t.Fatal("rebinding an existing key is neutral, no transitions")
	}
	_ = c.UnwindTo(mark) // v2 的逆把 a 恢复成 "v"——仍在，中性
	if comp.deacts != 0 {
		t.Fatal("restore-to-present must not deactivate")
	}

	// 回到 a 未绑定的状态：最后一个绑定被撤销
	_ = c.UnwindTo(0)
	if comp.deacts != 1 {
		t.Fatalf("unbinding must deactivate once, got %d", comp.deacts)
	}
	if c.Components()[0].Active {
		t.Fatal("status must show inactive")
	}
}

// 无关 key 的变化是中性：不动组件。
func TestUnrelatedChangeIsNeutral(t *testing.T) {
	c := New("default")
	comp := &testComp{name: "dep-a", requires: []string{"a"}}
	c.RegisterComponent(comp)
	_ = Set(c, keyFor("a"), "v")
	_ = Set(c, keyFor("zzz"), "v")
	if comp.activates != 1 || comp.deacts != 0 {
		t.Fatalf("unrelated bind must be neutral, acts=%d deacts=%d", comp.activates, comp.deacts)
	}
}

// 激活回调里再 Set（重入）：分类要收敛到正确终态，不许半套。
func TestReentrantActivateConverges(t *testing.T) {
	c := New("default")
	inner := &testComp{name: "inner", requires: []string{"b"}}
	c.RegisterComponent(inner)

	reentrant := &reentrantComp{ctx: c, dep: "b"}
	c.RegisterComponent(reentrant)
	_ = Set(c, keyFor("a"), "v") // 触发 reentrant 激活，它在回调里绑 b

	if inner.activates != 1 {
		t.Fatalf("nested dep must end up activated, got %d", inner.activates)
	}
	if !c.Components()[1].Active || !c.Components()[0].Active {
		t.Fatalf("both components must be active: %+v", c.Components())
	}
}

type reentrantComp struct {
	ctx *Context
	dep string
	on  bool
}

func (r *reentrantComp) Name() string       { return "reentrant" }
func (r *reentrantComp) Requires() []string { return []string{"a"} }
func (r *reentrantComp) Activate(c *Context) error {
	r.on = true
	return Set(r.ctx, NewKey[string](r.dep), "bound-by-activate")
}
func (r *reentrantComp) Deactivate(c *Context) error { r.on = false; return nil }

// 激活失败：组件保持未激活，错误进状态（不静默）。
func TestActivateFailureIsVisible(t *testing.T) {
	c := New("default")
	comp := &testComp{name: "dep-a", requires: []string{"a"}, actErr: errors.New("boom")}
	c.RegisterComponent(comp)
	_ = Set(c, keyFor("a"), "v")
	st := c.Components()[0]
	if st.Active || st.LastError == "" {
		t.Fatalf("failed activation must stay inactive with error visible: %+v", st)
	}
}
