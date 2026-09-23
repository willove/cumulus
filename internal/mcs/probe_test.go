package mcs

import (
	"context"
	"strings"
	"testing"
)

// probe: how often do stage-1 CJK anchors actually land in
// the body, and does a window containing the known answer survive scoring?
// Run with: go test ./internal/mcs -run TestR3AnchorProbe -v
func TestR3AnchorProbe(t *testing.T) {
	handbook := "# 部署手册\n\n## 数据库\n系统部署在广州机房，端口是 8480。\n\n" +
		"--- page 2 ---\n## 连接池\n关键配置：连接池最大 128，超时 30 秒。\n\n## 其他\n无关段落填充文本 padding padding padding。\n"
	pool := "# 连接池专册\n大量填充无关文本 padding padding padding padding padding padding。\n" +
		"关键配置：连接池最大 128，超时 30 秒。\n大量填充无关文本 padding padding padding padding padding padding。\n"
	design := "# 网关设计\n连接池上限由 capacity 模块决定：默认 128，紧急时可调至 256。\n" +
		"部署时先查 capacity 再填连接池参数。\n"

	cases := []struct {
		query string
		body  string
		gold  string // substring the evidence window must contain
	}{
		{"连接池最大连接数是多少", handbook, "128"},
		{"连接池最大连接数是多少", pool, "128"},
		{"连接池参数在哪里配置", design, "128"},
		{"超时时间是多少", handbook, "30 秒"},
		{"部署在哪个机房", handbook, "广州"},
		{"网关端口是多少", design, "8480"},
		{"容量模块如何决定连接池", design, "capacity"},
	}
	sc := KeywordScorer{}
	s := New(DefaultConfig(), sc)
	totalFields, hitFields := 0, 0
	localized, sampled := 0, 0
	for _, c := range cases {
		runes := []rune(c.body)
		for _, w := range Fields(c.query) {
			totalFields++
			if strings.Contains(c.body, w) {
				hitFields++
			}
		}
		samples, err := s.SampleBody(context.Background(), c.query, c.body)
		if err != nil {
			t.Fatal(err)
		}
		if len(samples) == 0 {
			continue
		}
		sampled++
		for _, sm := range samples {
			if sm.Score >= 4 && strings.Contains(sm.Content, c.gold) {
				localized++
				break
			}
		}
		_ = runes
	}
	hitRate := float64(hitFields) / float64(totalFields)
	locRate := float64(localized) / float64(len(cases))
	t.Logf("R3 anchor probe: field hit-rate %.3f (%d/%d) · localization %.3f (%d/%d cases)",
		hitRate, hitFields, totalFields, locRate, localized, len(cases))
	// Field hit-rate is recorded, not gated: CJK bigram anchors are noisy
	// (noisy anchors) — that is why stage 1 keeps the stratified arm. The KPI is the
	// answer actually landing in a scored window.
	if locRate < 0.5 {
		t.Fatalf("localization rate too low: %.3f", locRate)
	}
}
