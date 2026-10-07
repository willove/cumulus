// Package corpus 的字符面：raw bytes → UTF-8，或**响亮地拒收**。
//
// 搬 cumulus internal/charset 的三条纪律（在 1251 本中文小说语料上
// 实测分层：GB18030 87% / UTF-8 5.4% / 不可解码 7.6%）：
//
//  1. **入库时一次转码**，查询路径永远是纯净 UTF-8——字符问题是数据
//     问题，在入口解决，不带到每个下游；
//  2. **绝不静默替换**：没有 errors="replace" 这条路。把 GBK 字节当
//     UTF-8 存进去，每个汉字变成一个 U+FFFD，文档能回答查询但不可读，
//     没有任何一环会报告（参考实现的失败模式，cumulus 的注释钉死）；
//  3. **血缘可审计**：输入字节的 sha256（SrcDigest）、原字节数、是否
//     转换过——产物文本证明不了它来自哪个文件，输入摘要可以。
//
// 无损性双重校验（cumulus 同款，理由照搬）：
//   - 快路：GB18030 解码器的唯一替换字符是 U+FFFD，输出无 U+FFFD
//     即可证无损（一次线性扫描）；
//   - 权威：把解出的文本重新编码回 GB18030 与输入比对——真正的"转
//     换无损"不变量。只在快路发现损伤时跑（失败路径才付费）。
package corpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// Tier 是解码结果，记在文档上——转换过的语料与原生的必须可区分。
type Tier string

const (
	TierUTF8        Tier = "utf-8"       // 本来就是合法 UTF-8，原样保留
	TierGB18030     Tier = "gb18030"     // 转码得来（GB18030 是 GBK/GB2312 超集）
	TierUndecodable Tier = "undecodable" // 两者都不是——拒收入库
)

// ErrUndecodable 是错误而非降级：替代方案（替换字符）会产出一个"看起来
// 存好了、能答查询、其实不可读"的文档。
var ErrUndecodable = errors.New("ingest: bytes are neither valid UTF-8 nor GB18030")

// Result 是解码产物加血缘。
type Result struct {
	Text      string
	Tier      Tier
	SrcBytes  int    // 输入字节数（转换前）
	SrcDigest string // 输入字节的 sha256——文件→文本的链接凭证
	Converted bool   // 字节变过没有（报告里可不 diff 文本就区分）
}

// Decode 把原始文档字节转成 UTF-8，或拒收。永不替换。
func Decode(raw []byte) (Result, error) {
	res := Result{SrcBytes: len(raw), SrcDigest: DigestHex(raw)}

	// BOM 是编码标记不是内容：剥掉，不入库（留着会让首词永远对不上）。
	body := bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(body) {
		res.Text = string(body)
		res.Tier = TierUTF8
		return res, nil
	}

	// x/text 的 GB18030 解码器**不报错**（截断/不可映射/非法尾字节全都
	// err=nil 吐 U+FFFD）——原样接进来就是 errors="replace" 穿马甲。
	// 所以：先快路扫 U+FFFD（无即可证无损），有损伤再权威回编码比对。
	out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), body)
	if err != nil {
		res.Tier = TierUndecodable
		return res, fmt.Errorf("%w: %v", ErrUndecodable, err)
	}
	if bytes.ContainsRune(out, '\uFFFD') {
		// 有替换字符：回编码比对，拿真正的判定
		back, _, berr := transform.Bytes(simplifiedchinese.GB18030.NewEncoder(), out)
		if berr != nil || !bytes.Equal(back, body) {
			res.Tier = TierUndecodable
			return res, fmt.Errorf("%w: lossy decode (re-encode mismatch)", ErrUndecodable)
		}
	}
	res.Text = string(out)
	res.Tier = TierGB18030
	res.Converted = true
	return res, nil
}

// DigestHex 是内容寻址/血缘共用的 sha256 hex（取前 16 位与 store 的
// id 口径一致；血缘用全量，另给 DigestFull）。
func DigestHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// DigestFull sha256 全量 hex（血缘记录用）。
func DigestFull(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
