package context

import "fmt"

// Activator 是可选组件对 context 的声明与生命周期。
//
// Requires 就是组件的依赖规格：全部绑定时它该活着，任一缺失时它该
// 停下。谁来判定？context。每次上下文变化（绑定/解绑）后，每个组件
// 被分类为激活/停用/中性三类之一，转换由分类驱动——组件自己不许猜
// 自己的状态，也不许在依赖没了以后继续跑（cumulus 的 vocab 桥接撤除、
// MCS 采样悄悄不触发，死在没有这个分类器）。
type Activator interface {
	Name() string
	Requires() []string
	// Activate 在从未满足到满足时调用。
	Activate(c *Context) error
	// Deactivate 在从满足到不满足时调用。返回错误也照常停用——
	// 清理不许讨价还价。
	Deactivate(c *Context) error
}

// ComponentState 是 status 面的渲染单元：组件活着没有、缺什么、
// 上次激活/停用有没有出错。
type ComponentState struct {
	Name      string
	Active    bool
	Missing   []string
	LastError string
}

type componentEntry struct {
	spec      Activator
	active    bool
	lastError string
}

// RegisterComponent 登记一个可选组件。登记时就分类一次（可能当场激活）。
func (c *Context) RegisterComponent(a Activator) {
	if a == nil {
		return
	}
	c.mu.Lock()
	c.components = append(c.components, &componentEntry{spec: a})
	c.mu.Unlock()
	c.reevaluate()
}

// Components 返回全部可选组件的状态（status 面用）。
func (c *Context) Components() []ComponentState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ComponentState, 0, len(c.components))
	for _, e := range c.components {
		var missing []string
		for _, k := range e.spec.Requires() {
			if _, ok := c.values[k]; !ok {
				missing = append(missing, k)
			}
		}
		out = append(out, ComponentState{
			Name:      e.spec.Name(),
			Active:    e.active,
			Missing:   missing,
			LastError: e.lastError,
		})
	}
	return out
}

// reevaluate 对每个组件按当前绑定分类并驱动转换。
//
// 重入处理：Activate 里可以再 Set（组件自己绑东西），那会再调
// reevaluate——用深度计数把后续重排延到最外层完成后一次跑完，
// 否则一次绑定可能触发半套激活。
func (c *Context) reevaluate() {
	c.reevalDepth++
	defer func() {
		c.reevalDepth--
		if c.reevalDepth == 0 && c.reevalDirty {
			c.reevalDirty = false
			c.reevaluate()
		}
	}()

	c.mu.RLock()
	entries := make([]*componentEntry, len(c.components))
	copy(entries, c.components)
	c.mu.RUnlock()

	for _, e := range entries {
		missing := c.missingKeys(e.spec.Requires())
		switch {
		case len(missing) == 0 && !e.active:
			if err := e.spec.Activate(c); err != nil {
				e.lastError = fmt.Sprintf("activate: %v", err)
				continue // 激活失败：保持未激活，错误进状态
			}
			e.lastError = ""
			e.active = true
		case len(missing) > 0 && e.active:
			if err := e.spec.Deactivate(c); err != nil {
				e.lastError = fmt.Sprintf("deactivate: %v", err)
			}
			e.active = false // 清理失败也停用：依赖没了就是没了
		}
	}
}

func (c *Context) missingKeys(keys []string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var missing []string
	for _, k := range keys {
		if _, ok := c.values[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}

// touch 在一次绑定/解绑之后调用，触发分类。
func (c *Context) touch() {
	if c.reevalDepth > 0 {
		c.reevalDirty = true // 重入中：标记，最外层会重排
		return
	}
	c.reevaluate()
}
