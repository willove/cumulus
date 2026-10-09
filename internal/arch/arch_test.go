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

// tooling 是**正交层**：引擎不得依赖它。这条已经并进 Check 的方向规则，
// 这里再钉一次"例外是显式且有界的"——例外表每加一条都要有理由，
// 否则"为了让门禁变绿"就会变成常规操作。
func TestToolingExceptionsAreExplicitAndNarrow(t *testing.T) {
	deps, err := RealGraph()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, d := range deps {
		if _, ok := LayerOf(d.To); !ok || LayerOfMust(d.To) != LayerTooling {
			continue
		}
		if allowedToolingUse(d.From, d.To) {
			allowed[d.From+" → "+d.To] = true
		}
	}
	// 引擎面（kernel/ports/capabilities/pipeline/host 里的非 cmd 包）不得在白名单里
	for key := range allowed {
		from := key[:strings.Index(key, " ")]
		if strings.HasPrefix(from, "cmd/") {
			continue // 工具宿主，合法
		}
		if from == "api" {
			// api 露读数接口是**有意的例外**，但只许依赖两个包
			for to := range map[string]bool{"api → knowledge": true, "api → learncore": true} {
				if key == to {
					continue
				}
			}
			continue
		}
		t.Errorf("例外白名单里不该有引擎包：%s（要加先写理由）", key)
	}
	// 反向：被依赖面必须真的在 tooling 层（防止有人把包挪层后白名单还留着）
	for _, pkg := range []string{"evalfcore", "calib", "evaldata", "learncore", "judge"} {
		if l := LayerOfMust(pkg); l != LayerTooling {
			t.Errorf("%s 应留在 tooling 层（评测/校准/学习，2026-10 起暂停投入），现在是 %s", pkg, l)
		}
	}
}

func LayerOfMust(pkg string) Layer {
	l, _ := LayerOf(pkg)
	return l
}
