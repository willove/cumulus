package failure

import "testing"

// AsksNumeric：数值诉求的标记词命中即可；时间问法不在此列
// （时敏是 GaRAGe 的 OUTDATED 轴，不是底物轴）。
func TestAsksNumeric(t *testing.T) {
	yes := []string{
		"专利期限是多少年",
		"连接池最大连接数是多少",
		"闯红灯罚款金额多少",
		"违约比例是多少",
		"每年利率几",
	}
	for _, q := range yes {
		if !AsksNumeric(q) {
			t.Errorf("AsksNumeric(%q) = false, want true", q)
		}
	}
	no := []string{
		"红灯表示什么",
		"发明创造的定义",
		"闯红灯怎么处罚",
		"什么时候生效", // 时间问法：不混进底物判据
	}
	for _, q := range no {
		if AsksNumeric(q) {
			t.Errorf("AsksNumeric(%q) = true, want false", q)
		}
	}
}

// HasNumeric：中文数字必须带计量单位才算——否则法条里"第X条"满篇都是，
// 判据恒真、底物错配永不触发（死标签换成哑标签）。
func TestHasNumeric(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"发明专利权的期限为二十年", true},
		{"连接池最大连接数默认为 100", true},
		{"罚款二百元", true},
		{"第四十二条 发明专利权", false}, // 序号不是测量值
		{"第二条 本法所称", false},
		{"红灯表示禁止通行", false},
	}
	for _, c := range cases {
		if got := HasNumeric(c.text); got != c.want {
			t.Errorf("HasNumeric(%q) = %v, want %v", c.text, got, c.want)
		}
	}
	if HasNumeric() {
		t.Error("HasNumeric() with no texts must be false")
	}
	if !HasNumeric("无数字", "十年") {
		t.Error("HasNumeric must scan every text")
	}
}
