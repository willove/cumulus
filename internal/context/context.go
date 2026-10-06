// Package context 是 cumulus 的唯一共享状态容器。
//
// 四条纪律落在本包（依据 docs/flow-grammar.md 不变量 1/2/4）：
//   - 一切共享状态绑在 typed key 上，只经 Set/Register 进入；
//   - 每次注册必须带逆（Release），Unwind 按 LIFO 回放；
//   - stage 运行期间进入守卫模式，写未声明的 key 直接报错（禁闭）；
//   - 每次迁移记录提交视图（CommittedView）。
package context

import (
	"fmt"
	"sync"
	"time"
)

// Realm 是隔离域：同一个 key 在不同 realm 下解析到不同数据集（多租户）。
type Realm string

// Key 是带类型的上下文 key。key 由各业务包自行登记（见各包 keys.go），
// 命名规则 "<域>.<名>"，全进程唯一。
type Key[T any] struct {
	name string
}

func NewKey[T any](name string) Key[T] { return Key[T]{name: name} }

func (k Key[T]) String() string { return k.name }

// Release 撤销一次注册。必须幂等：重复调用不得改变状态。
type Release func()

// Registration 一次可逆的上下文变更。Release 为 nil 的注册会被拒绝。
type Registration struct {
	Key     string
	Realm   Realm
	Note    string // 人读的说明，进审计
	Release Release
}

// CommittedView 一次迁移针对的环境快照。四版本缺一不可比（见流程文法 §五.4）。
type CommittedView struct {
	At              time.Time
	Realm           Realm
	Flow            string
	CorpusVersion   string
	ConfigVersion   string
	StrategyVersion string
	BeliefVersion   string
}

// Context 是单一中介。零值不可用，必须 New。
type Context struct {
	realm    Realm
	mu       sync.RWMutex
	values   map[string]any
	releases []Registration

	guard     map[string]bool // 当前 stage 允许写的 key 集；nil 表示无守卫
	guardName string

	views []CommittedView

	components  []*componentEntry // 可选组件（分类器驱动启停）
	reevalDepth int               // 重入计数：激活回调里再 Set 会重入
	reevalDirty bool              // 重入中发生过变化：最外层要重排
}

func New(realm Realm) *Context {
	return &Context{realm: realm, values: map[string]any{}}
}

func (c *Context) Realm() Realm { return c.realm }

// BeginStage 进入 stage 守卫：之后只有 writes 里的 key 允许被写。
func (c *Context) BeginStage(name string, writes []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guardName = name
	c.guard = make(map[string]bool, len(writes))
	for _, w := range writes {
		c.guard[w] = true
	}
}

// EndStage 退出守卫。
func (c *Context) EndStage() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guard = nil
	c.guardName = ""
}

func (c *Context) checkWrite(key string) error {
	if c.guard == nil {
		return nil
	}
	if !c.guard[key] {
		return fmt.Errorf("context: stage %q writes undeclared key %q (confinement)", c.guardName, key)
	}
	return nil
}

// Set 绑定一个 key，返回的注册记录 old→new 的逆。重复绑定同一 key 时，
// 旧值进逆函数（后进先出的嵌套可逆），对应单一来源纪律：一个 key 一个提供者。
func Set[T any](c *Context, k Key[T], v T) error {
	c.mu.Lock()
	if err := c.checkWrite(k.name); err != nil {
		c.mu.Unlock()
		return err
	}
	prev, existed := c.values[k.name]
	c.values[k.name] = v
	realm := c.realm
	c.releases = append(c.releases, Registration{
		Key: k.name, Realm: realm, Note: fmt.Sprintf("set %s", k.name),
		Release: func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if existed {
				c.values[k.name] = prev
			} else {
				delete(c.values, k.name)
			}
		},
	})
	c.mu.Unlock()
	c.touch() // 绑定变化：重分类可选组件
	return nil
}

// Get 读取 key。未绑定或类型不符都返回 ok=false——调用方必须显式处理缺失，
// 这是"不静默失效"的第一道闸。
func Get[T any](c *Context, k Key[T]) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.values[k.name]
	if !ok {
		var zero T
		return zero, false
	}
	tv, ok := v.(T)
	if !ok {
		var zero T
		return zero, false
	}
	return tv, true
}

// Register 登记一次自定义效应（如打开连接、起定时器）。Release 不可为 nil。
// 用于 context 不管理其生命周期的资源。
func (c *Context) Register(r Registration) error {
	if r.Release == nil {
		return fmt.Errorf("context: registration %q has no release (invariant 2)", r.Key)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkWrite(r.Key); err != nil {
		return err
	}
	if r.Realm == "" {
		r.Realm = c.realm
	}
	c.releases = append(c.releases, r)
	return nil
}

// Mark 返回当前注册深度，供 UnwindTo 做流程级反卷（只撤本流程的注册，
// 不动流程之前就存在的东西，例如摄取阶段建立的语料注册）。
func (c *Context) Mark() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.releases)
}

// UnwindTo 把注册回放到 mark（含）之后的所有注册，按 LIFO 应用逆。
// 返回被撤销的注册说明，供审计。
func (c *Context) UnwindTo(mark int) []string {
	c.mu.Lock()
	if mark < 0 || mark > len(c.releases) {
		c.mu.Unlock()
		return nil
	}
	doomed := c.releases[mark:]
	kept := c.releases[:mark]
	c.releases = kept
	c.mu.Unlock()

	notes := make([]string, 0, len(doomed))
	for i := len(doomed) - 1; i >= 0; i-- {
		if doomed[i].Release != nil {
			doomed[i].Release()
		}
		notes = append(notes, doomed[i].Note)
	}
	c.touch() // 解绑变化：重分类可选组件
	return notes
}

// Commit 记录一次迁移的提交视图。At 与 Realm 由 context 填写。
func (c *Context) Commit(v CommittedView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v.At = time.Now()
	v.Realm = c.realm
	c.views = append(c.views, v)
}

// Views 返回全部提交视图（审计/回放用）。
func (c *Context) Views() []CommittedView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CommittedView, len(c.views))
	copy(out, c.views)
	return out
}
