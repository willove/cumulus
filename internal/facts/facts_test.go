package facts

import "testing"

// 专词硬切缺陷：查询没声明清单时 K=1（"发明创造定义"切出发明/创造，
// early stop 引错法还只烧一半 token——cumulus 实测缺陷的修法）
func TestDecomposeDefaultsToK1(t *testing.T) {
	fx := Decompose("发明创造定义")
	if len(fx) != 1 || fx[0].Query != "发明创造定义" {
		t.Fatalf("无并列结构必须 K=1，got %v", fx)
	}
}

// 查询声明了清单才切（多问号也成立）
func TestDecomposeMultiQuestion(t *testing.T) {
	fx := Decompose("专利期限是多少年？什么时候生效？")
	if len(fx) != 2 {
		t.Fatalf("两个问号 = 两条事实，got %d", len(fx))
	}
}

// "或者"是条件不是清单：切开造出两条需求而原文只有一条
func TestAlternativeIsNotCoordination(t *testing.T) {
	fx := Decompose("数额特别巨大或者有其他特别严重情节的判什么刑")
	if len(fx) != 1 {
		t.Fatalf("或者是二选一条件不许切，got %d: %v", len(fx), fx)
	}
}

// 单字标记两侧各需两字（"参与违法怎么办"不许切出"参"）
func TestSingleCharMarkGuarded(t *testing.T) {
	fx := Decompose("参与违法怎么办")
	for _, f := range fx {
		if len([]rune(f.Query)) < minUnitRunes {
			t.Fatalf("不许出现碎片事实：%q", f.Query)
		}
	}
}

// 覆盖判定：占比过半 + 3字核心在窗内，两条同时满足（红灯教训：半个词
// "红灯"在窗里不许把"闯红灯怎么处罚"整条判成已覆盖）
func TestEvaluateNeedsCoreInWindow(t *testing.T) {
	fx := []Fact{{ID: "f1", Query: "闯红灯怎么处罚"}}
	// 窗里只有"红灯"二字：占比可能过半，但 3 字核心（闯红灯/红灯怎/灯怎么…）
	// 不在 → 未盖
	ws := []Window{{SourceID: "w1", Span: "rune[0:20]", Text: "红灯表示禁止通行，绿灯表示准许通行。"}}
	rep := Evaluate(fx, ws)
	if rep.Complete {
		t.Fatal("半个词在场不许判已覆盖（cumulus 的红灯教训）")
	}
	if len(rep.Missing) != 1 || rep.Missing[0] != "f1" {
		t.Fatalf("f1 必须在 missing：%+v", rep)
	}
	// 窗里有完整核心（"闯红灯"或"怎么处罚"任一带三字核心即可）→ 盖
	ws2 := []Window{{SourceID: "w2", Span: "rune[0:20]", Text: "对闯红灯行为可以处警告或者二百元以下罚款，怎么处罚按此执行。"}}
	rep2 := Evaluate([]Fact{{ID: "f1", Query: "闯红灯怎么处罚"}}, ws2)
	if !rep2.Complete {
		t.Fatalf("核心在场必须判已覆盖：%+v", rep2)
	}
}

// NearMiss：未盖事实要报最好接进度（weakest-requirement 停止信号要用）
func TestEvaluateReportsNearMiss(t *testing.T) {
	rep := Evaluate([]Fact{{ID: "f1", Query: "闯红灯怎么处罚"}},
		[]Window{{Text: "红灯 表示 禁止 通行"}})
	if rep.Weakest <= 0 || rep.Weakest >= 1 {
		t.Fatalf("NearMiss 应在 (0,1)：%v", rep.Weakest)
	}
}

// 冲突门：同一事实两个窗给不同的数 = contested；同值不报
func TestConflictsDetectNumericDisagreement(t *testing.T) {
	fx := []Fact{{ID: "f1", Query: "连接池最大连接数是多少"}}
	ws := []Window{
		{SourceID: "a", Text: "本规范规定连接池的最大连接数为100个。"},
		{SourceID: "b", Text: "本规范规定连接池的最大连接数为200个。"},
	}
	cs := Conflicts(fx, ws)
	if len(cs) != 1 || cs[0].FactID != "f1" {
		t.Fatalf("同事实不同数必须报冲突：%+v", cs)
	}
	// 同值不报
	same := []Window{
		{SourceID: "a", Text: "本规范规定连接池的最大连接数为100个。"},
		{SourceID: "b", Text: "连接池最大连接数按前条执行为100个。"},
	}
	if cs2 := Conflicts(fx, same); len(cs2) != 0 {
		t.Fatalf("同值不许报冲突：%+v", cs2)
	}
}
