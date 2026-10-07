package corpus

import "strings"

// Normalize 是入库规范化（cumulus source.Normalize 的移植）：
// NUL 剥除、CRLF/CR → LF、空白折叠（连续空白成一个空格，保留换行）、
// 去首尾空白。**在入库时做一次**——查询路径拿到的文本就是规范的，
// 每个下游（分词/窗口/引用坐标）不必各自处理。
func Normalize(body string) string {
	body = strings.ReplaceAll(body, "\x00", "")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	var b strings.Builder
	b.Grow(len(body))
	prevSpace := false
	for _, r := range body {
		if r == '\n' {
			b.WriteRune(r)
			prevSpace = false
			continue
		}
		if isSpaceRune(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}

// isSpaceRune 与 unicode.IsSpace 同义，只收本土实现需要的几类
// （不引 unicode 包也行，但语义照搬）。
func isSpaceRune(r rune) bool {
	switch r {
	case ' ', '\t', '\v', '\f', 0x85, 0xA0:
		return true
	}
	return r == 0x1680 || (r >= 0x2000 && r <= 0x200A) || r == 0x2028 || r == 0x2029 || r == 0x202F || r == 0x205F || r == 0x3000
}
