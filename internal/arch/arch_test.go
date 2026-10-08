package arch

import (
	"strings"
	"testing"
)

// 规则本身（用合成图钉，不依赖仓库状态）：
//   - 同层允许（cmd → api；能力件互依）
//   - 反向禁止（kernel → pipeline、capability → apps、pipeline → capability…）
func TestLayerRules(t *testing.T) {
	cases := []struct {
		name    string
		deps    []Dependency
		wantBad bool
	}{
		{"下层被上层依赖是正常的", []Dependency{{"qaflow", "retrieval"}, {"retrieval", "prior"}, {"flow", "context"}}, false},
		{"同层允许", []Dependency{{"cmd/cumulus", "api"}, {"retrieval", "query"}}, false},
		{"cmd/** 按前缀归应用层（新增 cmd 包不必改表）", []Dependency{{"cmd/anything", "qaflow"}}, false},
		{"kernel 不得反向依赖流程", []Dependency{{"context", "qaflow"}}, true},
		{"能力件不得依赖应用面", []Dependency{{"retrieval", "api"}}, true},
		{"流程不得依赖能力件之外的下层（反了）", []Dependency{{"qaflow", "context"}}, false}, // 下层：允许
		{"端口不得被 kernel 依赖", []Dependency{{"context", "store"}}, true},
		{"能力件不得依赖流程", []Dependency{{"facts", "qaflow"}}, true},
	}
	for _, c := range cases {
		got := Check(c.deps)
		if (len(got) > 0) != c.wantBad {
			t.Errorf("%s: 期望违规=%v，实际 %v", c.name, c.wantBad, FormatViolations(got))
		}
	}
}

// **门禁本体**：对着真实导入图跑一遍。任何方向漂移或新包未登记都会红。
func TestLayerDirectionHolds(t *testing.T) {
	deps, err := RealGraph()
	if err != nil {
		t.Fatal(err)
	}
	if v := Check(deps); len(v) > 0 {
		t.Fatalf("依赖方向被打破（%d 条）：\n%s", len(v), FormatViolations(v))
	}
}

// 新包必须声明归属：不登记 = 没有约束 = 这道门禁形同虚设。
func TestEveryInternalPackageIsRegistered(t *testing.T) {
	pkgs, err := InternalPackages()
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for _, p := range pkgs {
		if _, ok := LayerOf(p); !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("这些包没在 internal/arch 的分层表里登记（新增包必须声明归属）：%s",
			strings.Join(missing, ", "))
	}
}

// 分层表里不能有**幽灵包**（登记了但已不存在）——否则表会慢慢失真。
func TestNoGhostPackagesInTable(t *testing.T) {
	pkgs, err := InternalPackages()
	if err != nil {
		t.Fatal(err)
	}
	exists := map[string]bool{}
	for _, p := range pkgs {
		exists[p] = true
	}
	for _, p := range Registered() {
		if !exists[p] {
			t.Errorf("分层表里的 %q 在仓库里不存在（表该更新了）", p)
		}
	}
}
