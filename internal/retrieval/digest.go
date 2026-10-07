package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// Digest 是语料的内容摘要：文档按 id 排序后，id + 正文一起做 sha256。
//
// 用途是提交视图里的**语料版本**（不变量 4）：不是时间戳、不是导入次序，
// 是内容寻址——同一份语料重导摘要不变（导入幂等，摘要也该幂等），
// 改一个字就变。个人库的语料一直在长，"这个答案是针对哪版语料给的"
// 必须能从答案本身查出来，而不是靠记忆。
func (idx *Index) Digest() string {
	if idx == nil || len(idx.byID) == 0 {
		return "empty"
	}
	ids := make([]string, 0, len(idx.byID))
	for id := range idx.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		d := idx.byID[id]
		h.Write([]byte(id))
		h.Write([]byte{0})
		h.Write([]byte(d.Body))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ShortDigest 取内容摘要前 12 位（渲染/日志用；完整摘要留给需要比对的场合）。
func (idx *Index) ShortDigest() string {
	d := idx.Digest()
	if len(d) <= 12 {
		return d
	}
	return d[:12]
}
