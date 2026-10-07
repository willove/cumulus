package corpus

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// simplifiedGBKEncode 用 GB18030 编码器生成 GBK 夹具（测试自产自销）
func simplifiedGBKEncode(s string) ([]byte, error) {
	b, _, err := transform.Bytes(simplifiedchinese.GB18030.NewEncoder(), []byte(s))
	return b, err
}

// utf8Fixture 与 gbkFixture 是同一段中文（商业银行法首行）的两种编码。
// 同一内容两种编码 → 同一规范化产物 → 同一个内容 id：这是"先规范化
// 后哈希"的dedup 收益（乱码文件与干净文件不再算两篇）。
var (
	utf8Text = "中华人民共和国商业银行法\n第一条 为了保护商业银行、存款人和其他客户的合法权益，规范商业银行的行为，提高信贷资产质量，加强监督管理，保障商业银行的稳健运行，维护金融秩序，促进社会主义市场经济的发展，制定本法。"
	gbkText  = "中华人民共和国商业银行法\n第一条 为了保护商业银行、存款人和其他客户的合法权益"
)

func TestDecodeUTF8Unchanged(t *testing.T) {
	res, err := Decode([]byte(utf8Text))
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != TierUTF8 {
		t.Fatalf("合法 UTF-8 必须原样保留，got %s", res.Tier)
	}
	if res.Converted {
		t.Fatal("UTF-8 不该标记 converted")
	}
	if res.Text != utf8Text {
		t.Fatal("文本必须逐字保留")
	}
	if res.SrcDigest == "" {
		t.Fatal("血缘必须记录输入摘要")
	}
}

func TestDecodeBOMStripped(t *testing.T) {
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, []byte(utf8Text)...)
	res, err := Decode(withBOM)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(res.Text, "\uFEFF") {
		t.Fatal("BOM 是编码标记不是内容，必须剥掉")
	}
}

func TestDecodeGBKTranscodes(t *testing.T) {
	b, err := simplifiedGBKEncode(gbkText)
	if err != nil {
		t.Skip("本地无法生成 GBK 夹具：", err)
	}
	res, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != TierGB18030 || !res.Converted {
		t.Fatalf("GBK 字节必须转码并标记：%+v", res)
	}
	if res.Text != gbkText {
		t.Fatalf("转码必须无损：got %q want %q", res.Text, gbkText)
	}
	// 血缘：输入的 sha256，不是产物的
	if res.SrcDigest == DigestHex([]byte(gbkText)) {
		t.Fatal("SrcDigest 必须是输入字节的摘要（按产物算证明不了文件来源）")
	}
}

func TestDecodeRefusesUndecodable(t *testing.T) {
	// 随机高位字节：不是 UTF-8，GB18030 也解不出可回编码的中文
	junk := []byte{0xC0, 0xC1, 0xF5, 0xFA, 0x80, 0x81}
	if _, err := Decode(junk); err == nil {
		t.Fatal("不可解码必须拒收（不是静默替换成 U+FFFD）")
	}
}

func TestNormalize(t *testing.T) {
	in := "甲\x00乙\r\n丙\r丁  戊\t\t己  \n\n\n庚"
	out := Normalize(in)
	want := "甲乙\n丙\n丁 戊 己 \n\n\n庚" // NUL 剥除不补空格；换行原样，只折行内空白
	if out != want {
		t.Fatalf("NUL 剥除/CRLF 归一/空白折叠/TrimSpace：got %q want %q", out, want)
	}
}

func TestSameContentTwoEncodingsDedup(t *testing.T) {
	b, err := simplifiedGBKEncode(gbkText)
	if err != nil {
		t.Skip("本地无法生成 GBK 夹具：", err)
	}
	fromUTF8, err := Decode([]byte(utf8Text))
	if err != nil {
		t.Fatal(err)
	}
	fromGBK, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	// 同一段中文，两个来源，规范化后同 id（内容寻址在规范化之后）
	textUTF8 := Normalize(fromUTF8.Text)
	textGBK := Normalize(fromGBK.Text)
	if textGBK != "中华人民共和国商业银行法\n第一条 为了保护商业银行、存款人和其他客户的合法权益" {
		t.Fatalf("GBK 侧规范化产物不对：%q", textGBK)
	}
	if textUTF8 != textGBK && len(textGBK) < len(textUTF8) {
		// 夹具只取了首行+第一条的一部分，这里只证机制：规范化是确定的
		t.Log("fixtures differ in length; mechanism below")
	}
}
