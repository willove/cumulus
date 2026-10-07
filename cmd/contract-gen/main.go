// Command contract-gen 从 Go 结构体生成 HTTP 契约产物（OpenAPI 3 + TS 类型）。
//
// 为什么是生成而不是手写：cumulus 的字段漂移事故（前端读 answer.conf、
// 后端字段是 confidence；conf/confidence 两份口径）根因是契约没有机器
// 强制——文档会过期，代码不会撒谎。契约从代码反射生成，产物陈旧即门禁红。
//
// 用法：
//
//	contract-gen            # 写到 stdout/指定目录（-o）或就地（-w）更新 docs/contract
//	contract-gen -w         # 就地更新产物（改契约形状时用）
//	contract-gen -check     // 只校验产物是否陈旧（门禁用）：陈旧 exit 2
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/willove/cumulus/internal/api"
	"github.com/willove/cumulus/internal/qaflow"
)

func main() {
	out := flag.String("o", "", "output directory (default stdout)")
	write := flag.Bool("w", false, "write artifacts into docs/contract")
	check := flag.Bool("check", false, "check artifacts are fresh (gate mode): exit 2 when stale")
	flag.Parse()

	openapi := genOpenAPI()
	ts := genTS()

	if *check || *write {
		dir := "docs/contract"
		if *out != "" {
			dir = *out
		}
		if *check {
			if stale(dir, openapi, ts) {
				fmt.Println("contract: artifacts are stale — run `go run ./cmd/contract-gen -w`")
				os.Exit(2)
			}
			fmt.Println("contract: artifacts fresh")
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal(err)
		}
		must(os.WriteFile(filepath.Join(dir, "openapi.json"), []byte(openapi+"\n"), 0o644))
		must(os.WriteFile(filepath.Join(dir, "types.ts"), []byte(ts+"\n"), 0o644))
		fmt.Printf("contract: wrote %s/{openapi.json,types.ts}\n", dir)
		return
	}
	fmt.Println(openapi)
	fmt.Println(ts)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "contract-gen:", err)
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fatal(err)
	}
}

func stale(dir, openapi, ts string) bool {
	for name, want := range map[string]string{"openapi.json": openapi, "types.ts": ts} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || strings.TrimSpace(string(got)) != strings.TrimSpace(want) {
			return true
		}
	}
	return false
}

// ---------- OpenAPI ----------

func genOpenAPI() string {
	doc := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "cumulus qa api",
			"version":     "0.1.0",
			"description": "从 Go 结构体反射生成；契约的裁判是代码，不是本文档。改形状先改代码。",
		},
		"paths": map[string]any{
			"/v1/qa": map[string]any{
				"post": map[string]any{
					"summary": "一次问答：返回完整 committed view（答案只是其中一个字段）",
					"requestBody": map[string]any{
						"required": true,
						"content":  map[string]any{"application/json": map[string]any{"schema": ref("QARequest")}},
					},
					"responses": map[string]any{
						"200":     resp("QAResponse"),
						"400":     errResp("请求不是合法 JSON 或 question 为空"),
						"500":     errResp("流程失败"),
						"default": errResp(""),
					},
				},
			},
			"/v1/health": map[string]any{
				"get": map[string]any{
					"summary":   "探活：语料量与 realm",
					"responses": map[string]any{"200": resp("HealthResponse"), "default": errResp("")},
				},
			},
			"/v1/status": map[string]any{
				"get": map[string]any{
					"summary":   "可选组件启停（分类器可见面）",
					"responses": map[string]any{"200": resp("StatusResponse"), "default": errResp("")},
				},
			},
		},
	}
	// 收全体型：请求/响应引用的全部结构体（含嵌套）
	schemas := map[string]any{}
	collect(reflect.TypeOf(api.QARequest{}), schemas)
	collect(reflect.TypeOf(api.QAResponse{}), schemas)
	collect(reflect.TypeOf(api.HealthResponse{}), schemas)
	collect(reflect.TypeOf(api.StatusResponse{}), schemas)
	doc["components"] = map[string]any{"schemas": schemas}
	b, err := json.MarshalIndent(doc, "", "  ")
	must(err)
	return string(b)
}

func resp(name string) map[string]any {
	return map[string]any{
		"description": "ok",
		"content":     map[string]any{"application/json": map[string]any{"schema": ref(name)}},
	}
}

func errResp(desc string) map[string]any {
	if desc == "" {
		desc = "error"
	}
	return map[string]any{"description": desc}
}

func ref(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// collect 递归收集体型（结构体字段引用的结构体也要进 schemas）。
func collect(t reflect.Type, schemas map[string]any) {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	name := t.Name()
	if name == "" || name == "Time" {
		return
	}
	if _, done := schemas[name]; done {
		return
	}
	schemas[name] = nil // 占位防递归环
	props := map[string]any{}
	required := []string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // 未导出
		}
		jsonName, omitempty := jsonField(f)
		if jsonName == "-" {
			continue
		}
		props[jsonName] = schemaOf(f.Type, schemas)
		if !omitempty {
			required = append(required, jsonName)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	// 描述从代码注释取不了——reflect 看不到 doc 注释，go/doc 要解析源码，
	// 生成器不背这个负担。字段语义写在 Go 注释里，人不靠 OpenAPI 学它。
	schemas[name] = schema
}

// schemaOf 把 Go 类型映射成 OpenAPI schema。
func schemaOf(t reflect.Type, schemas map[string]any) map[string]any {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaOf(t.Elem(), schemas)}
	case reflect.Map:
		return map[string]any{"type": "object"}
	case reflect.Struct:
		name := t.Name()
		if name == "" || name == "Time" {
			return map[string]any{"type": "object"}
		}
		collect(t, schemas)
		return ref(name)
	default:
		return map[string]any{}
	}
}

// jsonField 解析 json tag：名字 + 是否 omitempty。
func jsonField(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, false
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "" {
		name = f.Name
	}
	omit := false
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omit = true
		}
	}
	return name, omit
}

// ---------- TypeScript ----------

func genTS() string {
	var b strings.Builder
	b.WriteString("// 从 Go 结构体反射生成（cmd/contract-gen）——改形状先改 Go 代码，\n")
	b.WriteString("// 然后 `go run ./cmd/contract-gen -w`。手改本文件会被门禁判陈旧。\n\n")
	types := []reflect.Type{
		reflect.TypeOf(api.QARequest{}),
		reflect.TypeOf(api.QAResponse{}),
		reflect.TypeOf(api.HealthResponse{}),
		reflect.TypeOf(api.StatusResponse{}),
	}
	// 连带嵌套：把响应里引用的 qaflow 体也收进来
	for _, t := range []reflect.Type{
		reflect.TypeOf(qaflow.RouteSignals{}),
		reflect.TypeOf(qaflow.EscalationRecord{}),
		reflect.TypeOf(qaflow.ReuseState{}),
		reflect.TypeOf(qaflow.RerankState{}),
	} {
		types = append(types, t)
	}
	emitted := map[string]bool{}
	var order []reflect.Type
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || t.Name() == "" {
			return
		}
		if emitted[t.Name()] {
			return
		}
		emitted[t.Name()] = true
		order = append(order, t)
		for i := 0; i < t.NumField(); i++ {
			walk(t.Field(i).Type)
		}
	}
	for _, t := range types {
		walk(t)
	}
	for _, t := range order {
		writeTSInterface(&b, t)
	}
	return b.String()
}

func writeTSInterface(b *strings.Builder, t reflect.Type) {
	fmt.Fprintf(b, "export interface %s {\n", t.Name())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, omit := jsonField(f)
		if name == "-" {
			continue
		}
		fmt.Fprintf(b, "  %s%s: %s;\n", name, optMark(omit), tsType(f.Type))
	}
	b.WriteString("}\n\n")
}

func optMark(omit bool) string {
	if omit {
		return "?"
	}
	return ""
}

func tsType(t reflect.Type) string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "number"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return tsType(t.Elem()) + "[]"
	case reflect.Map:
		return "Record<string, unknown>"
	case reflect.Struct:
		if n := t.Name(); n != "" && n != "Time" {
			return n
		}
		return "Record<string, unknown>"
	default:
		return "unknown"
	}
}
